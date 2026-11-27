# iStoreOS / OpenWrt 包定义（ADR-029）：24.10 SDK 出 ipk，25.12 SDK 出 apk。不用 SDK 时用 scripts/build-istore.sh 直接打包。
# postinst / prerm 改动时同步 scripts/build-istore.sh。
# 二进制在 SDK 外预先交叉编译（scripts/build.sh 会放到 files/boxctl），
# 这里只负责打包，避免依赖 SDK 内的 Go/Node 版本。

include $(TOPDIR)/rules.mk

PKG_NAME:=boxctl
PKG_VERSION:=0.1.0
PKG_RELEASE:=1

include $(INCLUDE_DIR)/package.mk

define Package/boxctl
  SECTION:=net
  CATEGORY:=Network
  TITLE:=Banbu Box (web panel + Mihomo + proxy-guard)
  # Mihomo 编进二进制（ADR-030）：TUN 接管局域网；dnsmasq-full 的 nftset 填充 guard 的严格集合（ADR-032）
  DEPENDS:=@x86_64 +kmod-tun +dnsmasq-full +nftables +ca-bundle
endef

define Package/boxctl/description
  端侧软路由盒子管理程序：Web 管理面板（内置 Mihomo 内核）与独立的 proxy-guard 保护守护进程。
endef

define Package/boxctl/conffiles
/etc/config/boxctl
endef

define Build/Compile
endef

define Package/boxctl/install
	$(INSTALL_DIR) $(1)/usr/bin $(1)/etc/init.d $(1)/etc/config $(1)/lib/upgrade/keep.d
	$(INSTALL_BIN) ./files/boxctl $(1)/usr/bin/boxctl
	$(INSTALL_BIN) ./files/boxctl-guard.init $(1)/etc/init.d/boxctl-guard
	$(INSTALL_BIN) ./files/boxctl-panel.init $(1)/etc/init.d/boxctl-panel
	$(INSTALL_BIN) ./files/boxctl-core.init $(1)/etc/init.d/boxctl-core
	$(INSTALL_CONF) ./files/boxctl.config $(1)/etc/config/boxctl
	# sysupgrade 保留数据目录（数据库、设备密钥、证书）
	$(INSTALL_DATA) ./files/boxctl.keep $(1)/lib/upgrade/keep.d/boxctl
endef

define Package/boxctl/postinst
#!/bin/sh
[ -n "$${IPKG_INSTROOT}" ] && exit 0
/etc/init.d/boxctl-guard enable
/etc/init.d/boxctl-guard restart
# 内核是独立服务（ADR-045）：升级时不重启它，连接不断；新版内核在面板里点「重启内核」后生效。
# 从旧版（内核是面板的子进程）升级时，旧面板退出会带走内核，新面板按设置用服务把它拉起来。
/etc/init.d/boxctl-core enabled && /etc/init.d/boxctl-core start
/etc/init.d/boxctl-panel enable
/etc/init.d/boxctl-panel restart
exit 0
endef

# 卸载时停服务并清理 guard 的 nft 表和 dnsmasq 片段（M1-09）
define Package/boxctl/prerm
#!/bin/sh
[ -n "$${IPKG_INSTROOT}" ] && exit 0
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
if [ "$$(uci -q get dhcp.@dnsmasq[0].boxctl_boguspriv)" = 1 ]; then
	uci -q delete dhcp.@dnsmasq[0].boxctl_boguspriv
	uci -q set dhcp.@dnsmasq[0].boguspriv='1'
	dns=restart
fi
if [ -n "$$dns" ]; then
	uci -q commit dhcp
	/etc/init.d/dnsmasq $$dns
fi
exit 0
endef

$(eval $(call BuildPackage,boxctl))
