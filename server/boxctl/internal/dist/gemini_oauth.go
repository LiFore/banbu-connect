package dist

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ---- Antigravity 的 OAuth 客户端（BXC-F0-020）----
//
// 客户端自己跑一遍 Google OAuth 拿 refresh token，用的是 Antigravity 应用的 installed-app 客户端：
// client_id 是 cockpit-tools 读出的公开值，client_secret 同样写在 Antigravity 客户端里，由管理员在管理页面填写
// （不写进代码）。随同步下发给客户端，客户端用它登录和刷新访问令牌。
// 风险：借用官方客户端属于非官方行为，Google 可能据此封禁或更换客户端（见 docs/Antigravity实测.md）。

const (
	geminiOAuthKey = "gemini_oauth_client"
	// DefaultGeminiClientID 是 Antigravity 的 OAuth client_id。TODO(verify:V-agy-browser)
	DefaultGeminiClientID = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
)

// GeminiOAuth 是下发给客户端的 OAuth 客户端。
type GeminiOAuth struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// GeminiOAuthInfo 是管理页面看到的设置；secret 只返回有没有。
type GeminiOAuthInfo struct {
	ClientID  string `json:"client_id"`
	HasSecret bool   `json:"has_secret"`
}

func (s *Server) loadGeminiOAuth(ctx context.Context) (GeminiOAuth, error) {
	var stored struct {
		Sealed []byte `json:"sealed"`
	}
	if err := s.loadSetting(ctx, geminiOAuthKey, &stored); err != nil || len(stored.Sealed) == 0 {
		return GeminiOAuth{ClientID: DefaultGeminiClientID}, err
	}
	b, err := s.Secrets.Open(stored.Sealed, geminiOAuthKey)
	if err != nil {
		return GeminiOAuth{}, fmt.Errorf("解密 Antigravity OAuth 客户端失败：%w", err)
	}
	var c GeminiOAuth
	return c, json.Unmarshal(b, &c)
}

func (s *Server) GeminiOAuthInfo(ctx context.Context) (GeminiOAuthInfo, error) {
	c, err := s.loadGeminiOAuth(ctx)
	return GeminiOAuthInfo{ClientID: c.ClientID, HasSecret: c.ClientSecret != ""}, err
}

// SetGeminiOAuth 保存 OAuth 客户端。secret 为空表示不变。
func (s *Server) SetGeminiOAuth(ctx context.Context, clientID, secret string) (GeminiOAuthInfo, error) {
	clientID, secret = strings.TrimSpace(clientID), strings.TrimSpace(secret)
	if !strings.HasSuffix(clientID, ".apps.googleusercontent.com") {
		return GeminiOAuthInfo{}, &InputError{"客户端 ID 应以 .apps.googleusercontent.com 结尾"}
	}
	old, err := s.loadGeminiOAuth(ctx)
	if err != nil {
		return GeminiOAuthInfo{}, err
	}
	if secret == "" {
		secret = old.ClientSecret
	}
	if secret == "" {
		return GeminiOAuthInfo{}, &InputError{"请填写客户端密钥"}
	}
	b, _ := json.Marshal(GeminiOAuth{ClientID: clientID, ClientSecret: secret})
	sealed, err := s.Secrets.Seal(b, geminiOAuthKey)
	if err != nil {
		return GeminiOAuthInfo{}, err
	}
	if err := s.saveSetting(ctx, geminiOAuthKey, map[string][]byte{"sealed": sealed}); err != nil {
		return GeminiOAuthInfo{}, err
	}
	s.geminiChanged()
	return s.GeminiOAuthInfo(ctx)
}
