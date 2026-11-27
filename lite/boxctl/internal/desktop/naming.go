package desktop

// 产品名称是 Banbu Connect Lite：只做 AI 账号管理，不做代理。和完整版 Banbu Connect 可以同时安装，
// 所以数据目录、本机端口、更新通道都和完整版分开。内部命令仍是 boxctl desktop。
const (
	productName = "Banbu Connect Lite"
	// dirName 是用户数据目录的名称（令牌、绑定、日志都在里面）。
	dirName = "Banbu Connect Lite"
	// updateTargetPrefix 加在 Tauri updater 的 target 前（darwin → lite-darwin），
	// 管理服务按它把 Lite 版和完整版的更新包分开。
	updateTargetPrefix = "lite-"
)
