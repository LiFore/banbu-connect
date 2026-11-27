#!/usr/bin/env bash
# 构建桌面客户端：前端 → 内嵌进 boxctl → 打包进 Tauri 应用。在目标平台上运行（macOS 或 Windows 的 Git Bash）。
# with_gvisor 不能去掉：TUN 的 mixed 栈依赖它，缺了内核能运行，但 TUN 起不来。
#
# 打包后：仓库根目录有 .env（见 .env.example）且生成了更新包时，自动上传到管理服务（不发布），见 scripts/upload-release.sh。
#
# 用法：scripts/build-app.sh [版本号]
#   给了版本号（如 0.2.4）就先写进 tauri.conf.json 和 Cargo.toml，再打包；不给就沿用 tauri.conf.json 里的版本。
#   OTA 只认更高的版本：发布新包前要升版本号。
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
conf="$root/client/app/src-tauri/tauri.conf.json"
cargo="$root/client/app/src-tauri/Cargo.toml"

if [ $# -gt 1 ]; then
	echo "用法：$0 [版本号]" >&2
	exit 2
fi
if [ $# -eq 1 ]; then
	want="${1#v}"
	if ! [[ "$want" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
		echo "版本号要写成 x.y.z（如 0.2.4），收到：$1" >&2
		exit 2
	fi
	# 只改第一处 version：tauri.conf.json 顶层、Cargo.toml 的 [package]。
	sed -i.bak "1,/^ *\"version\": *\"[^\"]*\"/s/^\( *\"version\": *\"\)[^\"]*\"/\1$want\"/" "$conf"
	sed -i.bak "1,/^version *= *\"[^\"]*\"/s/^version *= *\"[^\"]*\"/version = \"$want\"/" "$cargo"
	rm -f "$conf.bak" "$cargo.bak"
	echo "版本号：${want}（已写入 tauri.conf.json、Cargo.toml）"
fi

(cd "$root/web" && npm ci && npm run build)

case "$(uname -s)" in
	Darwin) bin=boxctl ;;
	*) bin=boxctl.exe ;;
esac
resources="$root/client/app/src-tauri/resources"
mkdir -p "$resources"
# 服务的版本与应用一致：应用经 OTA 更新后，外壳比较两者，发现服务是旧的就提示重新安装。
version="$(sed -n 's/^ *"version": *"\([^"]*\)".*/\1/p' "$conf" | head -1)"
(cd "$root/boxctl" && CGO_ENABLED=0 go build -tags with_gvisor -trimpath \
	-ldflags "-s -w -X boxctl/internal/buildinfo.Version=$version" -o "$resources/$bin" ./cmd/boxctl)

# 随应用打包 geo 数据：安装时放进内核目录，第一次校验配置不用再下载（国内直连 jsDelivr 常失败）。
geo="https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest"
for pair in "GeoIP.dat:geoip-lite.dat" "GeoSite.dat:geosite.dat"; do
	name="${pair%%:*}"
	asset="${pair##*:}"
	if [ ! -s "$resources/$name" ] || [ -n "$(find "$resources/$name" -mtime +7 2>/dev/null)" ]; then
		curl -fsSL --retry 3 -o "$resources/$name.tmp" "$geo/$asset" && mv "$resources/$name.tmp" "$resources/$name" ||
			echo "警告：下载 $name 失败，安装包里不带 geo 数据，服务会在运行时下载" >&2
	fi
done

cd "$root/client/app"
npm install
npx tauri icon src-tauri/app-icon.png -o src-tauri/icons >/dev/null

# macOS：Tauri 自己打的 dmg 会被下面重做的覆盖，让它跳过 Finder 美化脚本（设了 CI 时 Tauri 给 create-dmg 加 --skip-jenkins）。
# 这一步在新版 macOS 上会报「不能将 statusbar visible … 设置为 false（-10006）」，整个构建跟着失败。
if [ "$(uname -s)" = Darwin ]; then
	export CI=true
	unset TAURI_BUNDLER_DMG_IGNORE_CI
fi

# 更新包签名：私钥只在构建机上（npx tauri signer generate -w ~/.tauri/banbu-connect.key 生成）。
# 没有私钥时照常打包，只是不生成更新包，不能上传到管理服务做 OTA。
key="${BANBU_SIGNING_KEY:-$HOME/.tauri/banbu-connect.key}"
if [ -f "$key" ]; then
	export TAURI_SIGNING_PRIVATE_KEY="$(cat "$key")"
	export TAURI_SIGNING_PRIVATE_KEY_PASSWORD="${TAURI_SIGNING_PRIVATE_KEY_PASSWORD:-}"
	npx tauri build
else
	echo "警告：没有找到签名私钥 ${key}，本次不生成 OTA 更新包" >&2
	npx tauri build --config '{"bundle":{"createUpdaterArtifacts":false}}'
fi

bundle="src-tauri/target/release/bundle"

# macOS：重做 dmg，加上背景提示、「修复无法打开」脚本和安装说明。应用只有 ad-hoc 签名、没有公证，
# 从浏览器下载后会提示「已损坏，无法打开」（知识库 K019），用户双击脚本去掉隔离标记即可。
# Tauri 的 dmg 配置不能放额外文件，这里用它生成的 bundle_dmg.sh（create-dmg）重做一份，覆盖原来的 dmg。
# 图标位置与 src-tauri/dmg/background.swift 画的背景对应，改布局时两边一起改（背景用 swift 重新生成）。
# --window-size 是 Finder 窗口外框（含标题栏），不是内容区：高度 = 背景布局 420 + 标题栏（新版 macOS 32px），
# 否则内容区比背景矮，底下两个图标露不全，一打开就有滚动条。
if [ "$(uname -s)" = Darwin ]; then
	dmgdir="$bundle/dmg"
	dmg="$(find "$dmgdir" -maxdepth 1 -name '*.dmg' | head -1)"
	if [ -n "$dmg" ] && [ -x "$dmgdir/bundle_dmg.sh" ]; then
		echo "== 重做 dmg（背景提示 + 修复脚本）"
		stage="$(mktemp -d)"
		ditto "$bundle/macos/Banbu Connect.app" "$stage/Banbu Connect.app"
		cp "src-tauri/dmg/修复无法打开.command" "src-tauri/dmg/安装说明.txt" "$stage/"
		chmod +x "$stage/修复无法打开.command"
		rm -f "$dmg"
		# 用 Tauri 生成的 create-dmg 的副本：Finder 脚本里隐藏工具栏、状态栏的几句包进 try。新版 macOS 的 Finder
		# 不让设 statusbar visible（-10006），这只影响窗口外观，失败了照样往下摆图标、设背景。
		# 放一个 .this-is-the-create-dmg-repo，让脚本从同目录的 support/ 读模板。
		tool="$(mktemp -d)"
		cp "$dmgdir/bundle_dmg.sh" "$tool/"
		cp -R "$bundle/share/create-dmg/support" "$tool/support"
		touch "$tool/.this-is-the-create-dmg-repo"
		perl -pi -e 's/^(\s*)(set (?:statusbar|toolbar) visible to false)\s*$/$1try\n$1\t$2\n$1end try\n/' "$tool/support/template.applescript"
		mkdmg() {
			"$tool/bundle_dmg.sh" --volname "Banbu Connect" --volicon "$dmgdir/icon.icns" \
				--background "src-tauri/dmg/background.tiff" --window-size 660 452 --icon-size 72 --text-size 12 \
				--icon "Banbu Connect.app" 165 150 --app-drop-link 495 150 \
				--icon "修复无法打开.command" 110 330 --hide-extension "修复无法打开.command" \
				--icon "安装说明.txt" 550 330 --hide-extension "安装说明.txt" \
				"$@" "$dmg" "$stage" >/dev/null
		}
		# Finder 脚本偶尔失败（窗口还没打开等）：清掉残留的临时镜像重试一次；还不行就打不带背景和图标布局的 dmg，
		# 不让整个构建失败，但要醒目提示（「修复无法打开」脚本还在，只是没有背景上的说明）。
		if ! mkdmg; then
			rm -f "$dmgdir"/rw.*.dmg "$dmg"
			echo "  Finder 美化脚本失败，重试一次" >&2
			sleep 3
			if ! mkdmg; then
				rm -f "$dmgdir"/rw.*.dmg "$dmg"
				echo "警告：dmg 的背景和图标布局没设上（Finder 脚本失败），改打不带美化的 dmg。" >&2
				echo "      先确认终端在「系统设置 › 隐私与安全性 › 自动化」里允许控制 Finder，再重新打包。" >&2
				mkdmg --skip-jenkins
			fi
		fi
		rm -rf "$stage" "$tool"
		echo "  $dmg"
	fi
fi

echo "产物在 client/app/$bundle/"
echo "上传到管理服务「客户端更新」的文件（版本 ${version}）："
find "$bundle" -maxdepth 2 \( -name '*.app.tar.gz' -o -name '*-setup.exe' -o -name '*.msi' \) -print 2>/dev/null | while read -r f; do
	[ -f "$f.sig" ] && echo "  $f  （签名：$f.sig）"
done

# 自动上传（不发布）。上传失败不影响本地产物，可以改好 .env 后单独运行 scripts/upload-release.sh。
if [ -f "$key" ] && [ -f "$root/.env" ]; then
	echo "== 上传到管理服务"
	"$root/scripts/upload-release.sh" || echo "警告：自动上传没有完成，可以在管理页面手动上传上面列出的文件" >&2
fi
