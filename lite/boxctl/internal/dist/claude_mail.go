package dist

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// ---- 查看 Claude 账号的邮件 ----
//
// 客户端可以查看账号 Gmail 里 Anthropic 发来的邮件（登录邮件、验证码、通知），不用打开 Gmail。
// 管理服务只返回 Anthropic / OpenAI 发来的邮件，正文已转成纯文本。
// 不强制走一键登录流程：自己手动登录的用户也能在这里看到登录邮件里的 magic link，复制或让托盘在浏览器打开。
// 注意 magic link 等同于登录凭据：被允许使用这个账号的客户端，在 7 天内都能看到。

// ClaudeMail 是一封邮件，正文为纯文本。
type ClaudeMail struct {
	ID      string    `json:"id"`
	From    string    `json:"from"`
	Subject string    `json:"subject"`
	Date    time.Time `json:"date"`
	Text    string    `json:"text"`
	Links   []string  `json:"links"` // 正文里的 magic link，单独列出方便打开和复制
	Codes   []string  `json:"codes"` // Codex 账号：正文里的 6 位验证码（BXC-F0-045）
}

// ClaudeMails 读取账号最近的 Anthropic 邮件（客户端）。
func (c *Client) ClaudeMails(ctx context.Context, accountID int64) ([]ClaudeMail, error) {
	var out []ClaudeMail
	err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("/api/v1/client/claude-accounts/%d/mails", accountID), nil, &out, requestTimeout+20*time.Second)
	if out == nil {
		out = []ClaudeMail{}
	}
	return out, err
}
