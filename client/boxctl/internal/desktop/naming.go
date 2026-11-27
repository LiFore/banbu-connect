package desktop

// 桌面客户端的产品名称是 Banbu Connect（班布）。服务名、安装目录、数据目录都用它；
// 内部命令仍是 boxctl desktop，与路由盒共用同一个二进制。
const (
	productName = "Banbu Connect"
	// dirName 是安装目录、数据目录和用户令牌目录的名称。
	dirName = "Banbu Connect"
)

// 改名前的服务和单独的 Gate 都写同一套系统规则，同时运行会互相覆盖，安装时要求先卸载。
const (
	legacyDarwinDesktopPlist = "/Library/LaunchDaemons/com.boxctl.desktop.plist"
	legacyDarwinGatePlist    = "/Library/LaunchDaemons/com.boxctl.gate.plist"
	legacyWindowsDesktop     = "boxctl"
	legacyWindowsGate        = "boxctl-gate"
)
