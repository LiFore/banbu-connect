#!/usr/bin/env bash
# 不用 OpenWrt SDK，直接打出路由盒可装的包（ADR-029）。按系统的包管理器分两个目标：
#
#   目标           包管理器  适用系统                              产物（build/istore/）
#   openwrt24.10   opkg      OpenWrt / iStoreOS 24.10 x86_64       boxctl_<版本>_x86_64.ipk、luci-app-boxctl_<版本>_all.ipk
#   openwrt25.12   apk       OpenWrt / iStoreOS 25.12 x86_64       boxctl-<apk版本>.apk、luci-app-boxctl-<apk版本>.apk
#
# 每个目标再打一个自解压安装包 banbu-box_<版本>_<目标>_x86_64.run：iStore「手动安装」上传它，或在 SSH 里 sh 它。
# .run 会检查系统的包管理器，装错版本时提示下载另一个。
#
# ipk 是外层 tar.gz，里面 debian-binary、control.tar.gz、data.tar.gz，脚本直接拼。
# apk 是 apk-tools 3 的二进制格式，只能用 `apk mkpkg` 生成：本机没有 apk-tools 3 时在 Docker 的 Alpine 容器里跑。
#
# 用法：scripts/build-istore.sh [--skip-build]   跳过时直接用 build/boxctl-linux-amd64
# 环境变量：
#   TARGETS="openwrt24.10 openwrt25.12"   只打其中一个目标时设置
#   VERSION=0.2.0                          指定版本号（默认从 git 推出）
#   APK_IMAGE=alpine:3.23                  打 apk 用的容器镜像（需要 apk-tools 3）
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/build/istore"
targets="${TARGETS:-openwrt24.10 openwrt25.12}"
apk_image="${APK_IMAGE:-alpine:3.23}"

for t in $targets; do
	case "$t" in openwrt24.10 | openwrt25.12) ;; *) echo "未知目标：$t（可选 openwrt24.10、openwrt25.12）" >&2; exit 1 ;; esac
done
want() { case " $targets " in *" $1 "*) return 0 ;; *) return 1 ;; esac; }

# 打 apk 的工具：本机的 apk-tools 3，或 Docker 里的 Alpine。先检查，免得编译完才发现打不了。
apk_native=""
if want openwrt25.12; then
	if command -v apk >/dev/null 2>&1 && apk mkpkg --help >/dev/null 2>&1; then
		apk_native=1
	elif ! docker info >/dev/null 2>&1; then
		echo "打 openwrt25.12（apk）需要 apk-tools 3 或可用的 Docker；只打 ipk 用 TARGETS=openwrt24.10" >&2
		exit 1
	fi
fi

if [ "${1:-}" != "--skip-build" ]; then
	bash "$root/scripts/build.sh"
fi
bin="$root/build/boxctl-linux-amd64"
[ -x "$bin" ] || { echo "缺少 $bin，先执行 scripts/build.sh" >&2; exit 1; }

# ---- 版本号 ----
raw="${VERSION:-$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo 0.1.0)}"
raw="${raw#v}"
# opkg 版本号只用数字、字母、点、加号、波浪号；git 描述里的其他字符换成加号。
version="$(printf '%s' "$raw" | sed 's/[^0-9A-Za-z.~+]/+/g')"
case "$version" in [0-9]*.[0-9]*) ;; *) version="0.1.0+$version" ;; esac
# 有未提交改动的构建每次都不同：版本号加上构建时间，否则包管理器认为已经是最新、不会安装。
case "$version" in *dirty) version="$version.$(date +%Y%m%d%H%M)" ;; esac

# apk 版本号格式严格（数字.数字[字母][_后缀数字][~十六进制]-r数字），不能有加号，单独推导：
#   打了标签的提交：0.2.0-r0
#   其他提交：<最近标签或 0.1.0>_p<时间>~<提交号>-r0，时间取提交时间，有未提交改动时取构建时间，保证新构建版本号更大。
apk_re='^[0-9]+(\.[0-9]+)*[a-z]?(_(alpha|beta|pre|rc|cvs|svn|git|hg|p)[0-9]*)*(~[0-9a-f]+)?(-r[0-9]+)?$'
if [ -n "${VERSION:-}" ]; then
	apk_version="${VERSION#v}"
	printf '%s' "$apk_version" | grep -Eq "$apk_re" || { echo "VERSION=$VERSION 不是合法的 apk 版本号（例：0.2.0、0.2.0_rc1）" >&2; exit 1; }
elif tag="$(git -C "$root" describe --tags --exact-match 2>/dev/null)" && [ -z "$(git -C "$root" status --porcelain 2>/dev/null)" ]; then
	apk_version="${tag#v}"
else
	base="$(git -C "$root" describe --tags --abbrev=0 2>/dev/null || echo 0.1.0)"
	base="${base#v}"
	printf '%s' "$base" | grep -Eq '^[0-9]+(\.[0-9]+)*$' || base=0.1.0
	if [ -n "$(git -C "$root" status --porcelain 2>/dev/null)" ]; then
		stamp="$(date +%Y%m%d%H%M)"
	else
		stamp="$(git -C "$root" log -1 --format=%cd --date=format:%Y%m%d%H%M 2>/dev/null || date +%Y%m%d%H%M)"
	fi
	hash="$(git -C "$root" rev-parse --short HEAD 2>/dev/null || true)"
	apk_version="${base}_p${stamp}${hash:+~$hash}"
fi
case "$apk_version" in *-r[0-9]*) ;; *) apk_version="$apk_version-r0" ;; esac

# 工作目录放在仓库的 build/ 下：Docker 容器要挂载它，系统临时目录不一定共享给容器。
mkdir -p "$root/build"
work="$(mktemp -d "$root/build/.istore-work.XXXXXX")"
trap 'rm -rf "$work"' EXIT
rm -rf "$out"
mkdir -p "$out"

# GNU tar 和 bsdtar（macOS）都要把属主写成 root，否则装到路由器上文件属于奇怪的用户。
# bsdtar 默认写 pax 扩展头（macOS 扩展属性），opkg 不认（get_header_tar: Unknown typeflag 0x78）：强制 gnutar 格式、不带扩展属性。
tar_root() {
	if tar --version 2>/dev/null | grep -q 'GNU tar'; then
		tar --owner=0 --group=0 --numeric-owner --format=gnu "$@"
	else
		tar --uid 0 --gid 0 --uname root --gname root --format gnutar --no-xattrs --no-acls --no-mac-metadata "$@"
	fi
}

installed_size() { du -sk --apparent-size "$1" 2>/dev/null | cut -f1 || du -sk "$1" | cut -f1; }

# ---- 包内容：两种格式共用同一份文件树和安装脚本 ----
boxctl_desc="Banbu Box：Web 管理面板、Mihomo 内核独立服务（boxctl-core）与独立的 proxy-guard 保护守护进程。"
luci_desc="Banbu Box 的 LuCI 入口（服务 → Banbu Box）。"

files="$root/openwrt/boxctl/files"
p="$work/boxctl"
install -d "$p/usr/bin" "$p/etc/init.d" "$p/etc/config" "$p/lib/upgrade/keep.d"
install -m 0755 "$bin" "$p/usr/bin/boxctl"
install -m 0755 "$files/boxctl-guard.init" "$p/etc/init.d/boxctl-guard"
install -m 0755 "$files/boxctl-panel.init" "$p/etc/init.d/boxctl-panel"
install -m 0755 "$files/boxctl-core.init" "$p/etc/init.d/boxctl-core"
install -m 0600 "$files/boxctl.config" "$p/etc/config/boxctl"
install -m 0644 "$files/boxctl.keep" "$p/lib/upgrade/keep.d/boxctl"

luci="$root/openwrt/luci-app-boxctl"
l="$work/luci"
install -d "$l/www"
cp -R "$luci/root/." "$l/"
cp -R "$luci/htdocs/." "$l/www/"

# 安装脚本与 openwrt/boxctl/Makefile 的 postinst / prerm 保持一致。
s="$work/scripts"
mkdir -p "$s"
cat >"$s/boxctl.postinst" <<'EOF'
#!/bin/sh
[ -n "${IPKG_INSTROOT}" ] && exit 0
/etc/init.d/boxctl-guard enable
/etc/init.d/boxctl-guard restart
# 内核是独立服务（ADR-045）：升级时不重启它，连接不断；新版内核在面板里点「重启内核」后生效。
# 从旧版（内核是面板的子进程）升级时，旧面板退出会带走内核，新面板按设置用服务把它拉起来。
/etc/init.d/boxctl-core enabled && /etc/init.d/boxctl-core start
/etc/init.d/boxctl-panel enable
/etc/init.d/boxctl-panel restart
exit 0
EOF
cat >"$s/boxctl.prerm" <<'EOF'
#!/bin/sh
[ -n "${IPKG_INSTROOT}" ] && exit 0
/etc/init.d/boxctl-panel stop
/etc/init.d/boxctl-panel disable
/etc/init.d/boxctl-core stop
/etc/init.d/boxctl-core disable
/etc/init.d/boxctl-guard stop
/etc/init.d/boxctl-guard disable
/usr/bin/boxctl guard -uninstall
# 删除发现路由盒用的 dnsmasq 记录（ADR-073）；恢复 boxctl 关掉的 boguspriv（ADR-081）
dns=
if uci -q get dhcp.boxctl_discovery >/dev/null; then
	uci -q delete dhcp.boxctl_discovery
	dns=reload
fi
if [ "$(uci -q get dhcp.@dnsmasq[0].boxctl_boguspriv)" = 1 ]; then
	uci -q delete dhcp.@dnsmasq[0].boxctl_boguspriv
	uci -q set dhcp.@dnsmasq[0].boguspriv='1'
	dns=restart
fi
if [ -n "$dns" ]; then
	uci -q commit dhcp
	/etc/init.d/dnsmasq $dns
fi
exit 0
EOF
cat >"$s/luci.postinst" <<'EOF'
#!/bin/sh
[ -n "${IPKG_INSTROOT}" ] && exit 0
rm -rf /tmp/luci-indexcache* /tmp/luci-modulecache
/etc/init.d/rpcd reload 2>/dev/null
exit 0
EOF
chmod 0755 "$s"/*

# ---- opkg：ipk ----
# make_ipk <文件树> <CONTROL 目录> <输出文件>
make_ipk() {
	local tree="$1" ctrl="$2" dest="$3" tmp
	tmp="$(mktemp -d "$work/ipk.XXXX")"
	echo "2.0" >"$tmp/debian-binary"
	(cd "$ctrl" && COPYFILE_DISABLE=1 tar_root -czf "$tmp/control.tar.gz" ./*)
	(cd "$tree" && COPYFILE_DISABLE=1 tar_root -czf "$tmp/data.tar.gz" .)
	(cd "$tmp" && COPYFILE_DISABLE=1 tar_root -czf "$dest" ./debian-binary ./control.tar.gz ./data.tar.gz)
}

if want openwrt24.10; then
	c="$work/ipk-control/boxctl"
	mkdir -p "$c"
	cat >"$c/control" <<EOF
Package: boxctl
Version: $version
Depends: libc, kmod-tun, dnsmasq-full, nftables, ca-bundle
Source: openwrt-customize
SourceName: boxctl
Section: net
Architecture: x86_64
Installed-Size: $(installed_size "$p")
Description: $boxctl_desc
EOF
	echo "/etc/config/boxctl" >"$c/conffiles"
	cp -p "$s/boxctl.postinst" "$c/postinst"
	cp -p "$s/boxctl.prerm" "$c/prerm"
	make_ipk "$p" "$c" "$out/boxctl_${version}_x86_64.ipk"

	c="$work/ipk-control/luci"
	mkdir -p "$c"
	cat >"$c/control" <<EOF
Package: luci-app-boxctl
Version: $version
Depends: libc, boxctl, luci-base
Source: openwrt-customize
SourceName: luci-app-boxctl
Section: luci
Architecture: all
Installed-Size: $(installed_size "$l")
Description: $luci_desc
EOF
	cp -p "$s/luci.postinst" "$c/postinst"
	make_ipk "$l" "$c" "$out/luci-app-boxctl_${version}_all.ipk"
fi

# ---- apk：apk-tools 3 ----
# apk 升级时跑 post-upgrade 而不是 post-install，两个都挂同一个脚本；卸载跑 pre-deinstall。
# 和官方包一样在 /lib/apk/packages/<包名>.conffiles 列出配置文件（sysupgrade 备份按它取文件）。
if want openwrt25.12; then
	install -d "$p/lib/apk/packages"
	echo "/etc/config/boxctl" >"$p/lib/apk/packages/boxctl.conffiles"
	cat >"$work/mkapk.sh" <<'EOF'
#!/bin/sh
# 在容器（或装有 apk-tools 3 的本机）里执行：mkapk.sh <工作目录> <输出目录> <apk版本>
set -e
w="$1" o="$2" v="$3"
stage="$(mktemp -d)"
cp -a "$w/boxctl" "$stage/boxctl"
cp -a "$w/luci" "$stage/luci"
# apk 记录文件的真实属主：统一改成 root（本机执行时没有 root 权限就跳过，由调用方保证）
chown -R 0:0 "$stage" 2>/dev/null || true
apk mkpkg \
	--info "name:boxctl" \
	--info "version:$v" \
	--info "arch:x86_64" \
	--info "description:$(cat "$w/boxctl.desc")" \
	--info "origin:openwrt-customize" \
	--info "depends:libc kmod-tun dnsmasq-full nftables ca-bundle" \
	--script "post-install:$w/scripts/boxctl.postinst" \
	--script "post-upgrade:$w/scripts/boxctl.postinst" \
	--script "pre-deinstall:$w/scripts/boxctl.prerm" \
	--files "$stage/boxctl" \
	--output "$o/boxctl-$v.apk"
apk mkpkg \
	--info "name:luci-app-boxctl" \
	--info "version:$v" \
	--info "arch:noarch" \
	--info "description:$(cat "$w/luci.desc")" \
	--info "origin:openwrt-customize" \
	--info "depends:libc boxctl luci-base" \
	--script "post-install:$w/scripts/luci.postinst" \
	--script "post-upgrade:$w/scripts/luci.postinst" \
	--files "$stage/luci" \
	--output "$o/luci-app-boxctl-$v.apk"
rm -rf "$stage"
EOF
	printf '%s' "$boxctl_desc" >"$work/boxctl.desc"
	printf '%s' "$luci_desc" >"$work/luci.desc"
	if [ -n "$apk_native" ]; then
		sh "$work/mkapk.sh" "$work" "$out" "$apk_version"
	else
		docker run --rm -v "$root/build:$root/build" "$apk_image" \
			sh "$work/mkapk.sh" "$work" "$out" "$apk_version"
	fi
fi

# ---- .run：自解压安装包 ----
# make_run <目标> <包管理器> <另一个目标>：把 build/istore 下该目标的包打进去。
make_run() {
	local target="$1" pm="$2" other="$3" payload run
	payload="$work/payload-$target"
	mkdir -p "$payload"
	case "$pm" in
		opkg) cp "$out"/*.ipk "$payload/" ;;
		apk) cp "$out"/*.apk "$payload/" ;;
	esac
	run="$out/banbu-box_${version}_${target}_x86_64.run"
	sed -e "s/@TARGET@/$target/g" -e "s/@PM@/$pm/g" -e "s/@OTHER@/$other/g" >"$run" <<'EOF'
#!/bin/sh
# Banbu Box 安装包（@TARGET@，@PM@）：解出软件包后用系统的包管理器安装，依赖从软件源获取。
set -e
[ "$(uname -m)" = "x86_64" ] || { echo "只支持 x86_64，当前是 $(uname -m)" >&2; exit 1; }

# OpenWrt 25.12 起用 apk，24.10 及以前用 opkg。
if command -v apk >/dev/null 2>&1; then
	PKG_MANAGER="apk"
elif command -v opkg >/dev/null 2>&1; then
	PKG_MANAGER="opkg"
else
	echo "没有找到 apk 或 opkg，不是 OpenWrt / iStoreOS 系统？" >&2
	exit 1
fi
if [ "$PKG_MANAGER" != "@PM@" ]; then
	echo "这个安装包用于 @PM@ 的系统（@TARGET@），当前系统用 $PKG_MANAGER。" >&2
	echo "请改用 banbu-box_<版本>_@OTHER@_x86_64.run。" >&2
	exit 1
fi

pkg_update() {
	"$PKG_MANAGER" update
}
pkg_installed() {
	case "$PKG_MANAGER" in
		apk) apk info -e "$1" >/dev/null 2>&1 ;;
		opkg) opkg list-installed | grep -q "^$1 " ;;
	esac
}
pkg_install() {
	case "$PKG_MANAGER" in
		apk) apk add "$@" ;;
		opkg) opkg install "$@" ;;
	esac
}
# 本地软件包没有官方签名：apk 要显式允许。
pkg_install_local() {
	case "$PKG_MANAGER" in
		apk) apk add --allow-untrusted "$@" ;;
		opkg) opkg install "$@" ;;
	esac
}
pkg_remove() {
	case "$PKG_MANAGER" in
		apk) apk del "$@" ;;
		opkg) opkg remove "$@" ;;
	esac
}

dir="$(mktemp -d /tmp/banbu-box.XXXXXX)"
trap 'rm -rf "$dir"' EXIT
line=$(awk '/^__PAYLOAD__$/ { print NR + 1; exit }' "$0")
tail -n +"$line" "$0" | tar -xzf - -C "$dir"

echo "包管理器：$PKG_MANAGER"
case "$PKG_MANAGER" in
	opkg)
		# dnsmasq-full 与默认的 dnsmasq 冲突，opkg 不会自动替换：先换掉（iStoreOS 默认已是 dnsmasq-full 时跳过）。
		if ! pkg_installed dnsmasq-full; then
			pkg_update
			pkg_remove dnsmasq
			pkg_install dnsmasq-full
		fi
		pkg_install_local "$dir"/boxctl_*.ipk "$dir"/luci-app-boxctl_*.ipk
		;;
	apk)
		# dnsmasq-full 声明了 provides dnsmasq，apk 在同一个事务里替换掉 dnsmasq，不用先删。
		pkg_update
		pkg_install_local "$dir"/boxctl-*.apk "$dir"/luci-app-boxctl-*.apk
		;;
esac
lan="$(uci -q get network.lan.ipaddr || echo '<LAN 地址>')"
echo "安装完成。面板：https://${lan%%/*}:$(uci -q get boxctl.panel.https_port || echo 8443)/"
echo "初始密码：$(cat /etc/boxctl/initial-password 2>/dev/null || echo '面板首次启动后见 /etc/boxctl/initial-password')"
exit 0
__PAYLOAD__
EOF
	(cd "$payload" && COPYFILE_DISABLE=1 tar_root -czf - ./*) >>"$run"
	chmod 0755 "$run"
}

want openwrt24.10 && make_run openwrt24.10 opkg openwrt25.12
want openwrt25.12 && make_run openwrt25.12 apk openwrt24.10

echo "已生成："
ls -l "$out"
