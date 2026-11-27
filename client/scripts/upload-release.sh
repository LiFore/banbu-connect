#!/usr/bin/env bash
# 把打好的 OTA 更新包上传到管理服务「客户端更新」（BXC-FY-053）。只上传、不发布：
# 上传后在管理页面左列「灰度与待发布」里，确认后再发布或设灰度名单。
#
# 配置在仓库根目录的 .env（不进 git，示例见 .env.example）：
#   BANBU_SERVER_URL          管理服务地址，如 https://203.0.113.10:8443
#   BANBU_ADMIN_PASSWORD      管理员密码
#   BANBU_SERVER_FINGERPRINT  服务端证书的 SHA-256 指纹（十六进制，管理页面「客户端」里绑定用的那个）；
#                             用自签名证书时必填，脚本先核对指纹再发密码。用正式 CA 证书时留空。
#   BANBU_RELEASE_NOTES       更新说明（可选）
#
# 用法：scripts/upload-release.sh [更新包文件...]
#   不给文件时，上传 client/app/src-tauri/target/release/bundle 下当前版本、当前平台的更新包。
#   每个文件旁边要有同名的 .sig 签名。
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
envfile="$root/.env"

# 只读 BANBU_ 开头的 KEY=VALUE，不执行 .env 里的内容；值两侧的引号去掉。
if [ -f "$envfile" ]; then
	while IFS= read -r line || [ -n "$line" ]; do
		line="${line%$'\r'}"
		[[ "$line" =~ ^[[:space:]]*(BANBU_[A-Z_]+)[[:space:]]*=(.*)$ ]] || continue
		key="${BASH_REMATCH[1]}"
		val="${BASH_REMATCH[2]}"
		val="${val#"${val%%[![:space:]]*}"}"
		val="${val%"${val##*[![:space:]]}"}"
		if [[ "$val" =~ ^\"(.*)\"$ || "$val" =~ ^\'(.*)\'$ ]]; then
			val="${BASH_REMATCH[1]}"
		fi
		[ -z "${!key:-}" ] && export "$key=$val"
	done <"$envfile"
fi

server="${BANBU_SERVER_URL:-}"
password="${BANBU_ADMIN_PASSWORD:-}"
fingerprint="$(echo "${BANBU_SERVER_FINGERPRINT:-}" | tr -d ':[:space:]' | tr 'A-F' 'a-f')"
notes="${BANBU_RELEASE_NOTES:-}"
if [ -z "$server" ] || [ -z "$password" ]; then
	echo "没有配置上传：在 $envfile 里填 BANBU_SERVER_URL 和 BANBU_ADMIN_PASSWORD（见 .env.example）" >&2
	exit 3
fi
server="${server%/}"
api="$server/api/v1"

# ---- 要上传的文件 ----
conf="$root/client/app/src-tauri/tauri.conf.json"
version="$(sed -n 's/^ *"version": *"\([^"]*\)".*/\1/p' "$conf" | head -1)"
case "$(uname -s)" in
	Darwin)
		case "$(uname -m)" in
			arm64) target=darwin-aarch64 ;;
			*) target=darwin-x86_64 ;;
		esac
		pattern='*.app.tar.gz'
		;;
	*)
		target=windows-x86_64
		pattern="*_${version}_*-setup.exe"
		;;
esac
files=("$@")
if [ ${#files[@]} -eq 0 ]; then
	bundle="$root/client/app/src-tauri/target/release/bundle"
	while IFS= read -r f; do files+=("$f"); done < <(find "$bundle" -maxdepth 2 -name "$pattern" 2>/dev/null)
fi
if [ ${#files[@]} -eq 0 ]; then
	echo "没有找到要上传的更新包（${pattern}）；先运行 scripts/build-app.sh，并确保有签名私钥" >&2
	exit 1
fi
for f in "${files[@]}"; do
	if [ ! -f "$f.sig" ]; then
		echo "缺少签名文件：$f.sig" >&2
		exit 1
	fi
done

# ---- 连接与证书 ----
tmp="$(mktemp -d)"
chmod 700 "$tmp"
jar="$tmp/cookies"
curl_tls=()
if [ -n "$fingerprint" ]; then
	# 自签名证书：取服务端证书核对指纹，再把它的公钥钉住（--pinnedpubkey），后面所有请求只认这把公钥。
	hostport="${server#*://}"
	hostport="${hostport%%/*}"
	host="${hostport%:*}"
	[ "$host" = "$hostport" ] && hostport="$hostport:443"
	if ! openssl s_client -connect "$hostport" -servername "$host" </dev/null 2>/dev/null | openssl x509 -outform PEM >"$tmp/cert.pem" 2>/dev/null; then
		echo "连不上管理服务 ${hostport}，或取不到证书" >&2
		rm -rf "$tmp"
		exit 1
	fi
	got="$(openssl x509 -in "$tmp/cert.pem" -outform DER | openssl dgst -sha256 | sed 's/^.*= *//' | tr 'A-F' 'a-f')"
	if [ "$got" != "$fingerprint" ]; then
		echo "服务端证书指纹不符，已停止（没有发送密码）：" >&2
		echo "  期望 $fingerprint" >&2
		echo "  实际 $got" >&2
		rm -rf "$tmp"
		exit 1
	fi
	pin="$(openssl x509 -in "$tmp/cert.pem" -pubkey -noout | openssl pkey -pubin -outform DER | openssl dgst -sha256 -binary | openssl base64)"
	curl_tls=(--insecure --pinnedpubkey "sha256//$pin")
fi
call() { curl -sS ${curl_tls[@]+"${curl_tls[@]}"} -b "$jar" -c "$jar" "$@"; }

csrf=""
cleanup() {
	# 退出登录，不在服务端留下会话；失败也不影响结果。
	if [ -n "$csrf" ]; then
		call -o /dev/null -X POST -H "X-CSRF-Token: $csrf" "$api/auth/logout" || true
	fi
	rm -rf "$tmp"
}
trap cleanup EXIT

# ---- 登录：密码经标准输入传给 curl，不出现在进程列表里 ----
esc="${password//\\/\\\\}"
esc="${esc//\"/\\\"}"
status="$(printf '{"password":"%s"}' "$esc" | call -o "$tmp/login.json" -w '%{http_code}' \
	-H 'Content-Type: application/json' --data-binary @- "$api/auth/login")" || {
	echo "连不上管理服务 $server" >&2
	exit 1
}
msg() { sed -n 's/.*"message":"\([^"]*\)".*/\1/p' "$1"; }
case "$status" in
	200) ;;
	401 | 403) echo "登录失败：管理员密码不对（连续错 5 次会锁定这个 IP 5 分钟，先核对 .env 再重试）" >&2; exit 1 ;;
	429) echo "登录失败：失败次数过多，已锁定，稍后再试" >&2; exit 1 ;;
	*) echo "登录失败：HTTP $status $(msg "$tmp/login.json")" >&2; exit 1 ;;
esac
csrf="$(sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p' "$tmp/login.json")"
if grep -q '"must_change":true' "$tmp/login.json"; then
	echo "管理员密码需要先在管理页面修改，改完后更新 .env 再上传" >&2
	exit 1
fi

# ---- 上传：文件放在表单最后（服务端边读边存） ----
for f in "${files[@]}"; do
	echo "上传 $(basename "$f")（${version}，${target}）……"
	status="$(call -o "$tmp/upload.json" -w '%{http_code}' -X POST -H "X-CSRF-Token: $csrf" \
		--form-string "version=$version" \
		--form-string "target=$target" \
		--form-string "notes=$notes" \
		--form-string "signature=$(cat "$f.sig")" \
		-F "file=@$f" \
		"$api/dist/releases")"
	if [ "$status" != 200 ]; then
		echo "上传失败：HTTP $status $(msg "$tmp/upload.json")" >&2
		exit 1
	fi
	echo "  已上传，未发布：到管理页面「客户端更新」的「灰度与待发布」里发布或设灰度名单"
done
