# LuCI 入口（ADR-029）：在「服务」菜单和 iStore「已安装」里显示 Banbu Box，打开面板。
# 面板本身由 boxctl 提供，这里只放菜单、权限和一个跳转页。

include $(TOPDIR)/rules.mk

LUCI_TITLE:=Banbu Box
LUCI_DEPENDS:=+boxctl
LUCI_PKGARCH:=all
PKG_VERSION:=0.3.0
PKG_RELEASE:=1

include $(TOPDIR)/feeds/luci/luci.mk

# call BuildPackage - OpenWrt buildroot signature
