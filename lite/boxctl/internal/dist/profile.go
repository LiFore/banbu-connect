// Package dist 是 Banbu Connect Lite 连接管理服务（boxctl server）的客户端：绑定与接入申请、
// 托管的 AI 账号（Claude / Codex / Gemini）、额度与在线上报、文件下载、应用更新。
//
// 管理服务下发的配置里还有代理用的节点、代理组和规则，Lite 版不做代理，只读版本信息。
package dist

import "time"

// Profile 是管理服务发布的一份配置。Lite 版只用版本信息，其余字段（节点、代理组、规则）忽略。
type Profile struct {
	Version     int64     `json:"version"`
	PublishedAt time.Time `json:"published_at"`
	Note        string    `json:"note"`
}
