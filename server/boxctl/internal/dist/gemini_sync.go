package dist

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// ---- 客户端同步 Gemini 账号与凭据（BXC-F0-018）----
//
// 客户端长轮询：带上次拿到的 hash，服务端在这台客户端应持有的账号、凭据、账号顺序或待执行指令变化时
// （或等满 wait）返回。服务端不记客户端拿到了什么，只比 hash，重启后客户端自然拿到全量。

const maxSyncWait = 30 * time.Second

// GeminiSyncAccount 是分配给这台客户端的账号；Credential 为空表示还没有人登录。
type GeminiSyncAccount struct {
	ID         int64           `json:"id"`
	Email      string          `json:"email"`
	Note       string          `json:"note"`
	Version    int64           `json:"version"`
	Credential json.RawMessage `json:"credential,omitempty"`
}

// GeminiSwitch 是管理页面下发的切换指令，客户端执行后回报 ID。
type GeminiSwitch struct {
	ID        string `json:"id"`
	AccountID int64  `json:"account_id"`
}

// GeminiSync 是一次同步的结果。Order 是建议的使用顺序（BXC-F0-023），只含有凭据的账号。
type GeminiSync struct {
	Hash     string              `json:"hash"`
	Accounts []GeminiSyncAccount `json:"accounts"`
	Order    []int64             `json:"order"`
	Switch   *GeminiSwitch       `json:"switch,omitempty"`
	// OAuth 是登录和刷新访问令牌用的 Antigravity OAuth 客户端；管理员还没填密钥时为空。
	OAuth *GeminiOAuth `json:"oauth,omitempty"`
	// Tests 是交给这台客户端执行的鉴权测试（ADR-061）。
	Tests []AuthTest `json:"tests,omitempty"`
}

// GeminiSync 返回这台客户端当前应持有的账号与凭据。
func (s *Server) GeminiSync(ctx context.Context, deviceID int64) (GeminiSync, error) {
	out := GeminiSync{Accounts: []GeminiSyncAccount{}, Order: []int64{}}
	rows, err := s.DB.QueryContext(ctx, `SELECT g.id, g.email, g.note, g.cred_version FROM gemini_accounts g
		JOIN gemini_assignments a ON a.account_id = g.id WHERE a.device_id = ? ORDER BY g.email`, deviceID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var a GeminiSyncAccount
		if err := rows.Scan(&a.ID, &a.Email, &a.Note, &a.Version); err != nil {
			rows.Close()
			return out, err
		}
		out.Accounts = append(out.Accounts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	for i := range out.Accounts {
		raw, _, err := s.geminiCredential(ctx, out.Accounts[i].ID)
		if err != nil {
			return out, err
		}
		out.Accounts[i].Credential = raw
	}
	if out.Order, err = s.geminiOrder(ctx, out.Accounts); err != nil {
		return out, err
	}
	var sw GeminiSwitch
	err = s.DB.QueryRowContext(ctx, `SELECT switch_id, switch_to FROM gemini_device_state WHERE device_id = ?`, deviceID).
		Scan(&sw.ID, &sw.AccountID)
	switch {
	case err == nil && sw.ID != "":
		out.Switch = &sw
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return out, err
	}
	oauth, err := s.loadGeminiOAuth(ctx)
	if err != nil {
		return out, err
	}
	if oauth.ClientSecret != "" && len(out.Accounts) > 0 {
		out.OAuth = &oauth
	}
	if t := s.deviceTests(deviceID, "gemini"); len(t) > 0 {
		out.Tests = t
	}
	body, _ := json.Marshal(out)
	sum := sha256.Sum256(body)
	out.Hash = hex.EncodeToString(sum[:12])
	return out, nil
}

// GeminiSyncWait 在结果的 hash 与 since 不同时立即返回，否则等变化，最多 wait。
func (s *Server) GeminiSyncWait(ctx context.Context, deviceID int64, since string, wait time.Duration) (GeminiSync, error) {
	wait = min(wait, maxSyncWait)
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		woke := s.geminiWaiter() // 先取通道再查：查询之后的变化也能唤醒
		out, err := s.GeminiSync(ctx, deviceID)
		if err != nil || out.Hash != since || wait <= 0 {
			return out, err
		}
		select {
		case <-woke:
		case <-deadline.C:
			return out, nil
		case <-ctx.Done():
			return out, nil
		}
	}
}

// GeminiStateReport 是客户端回报的当前账号；SwitchDone 是刚执行完的切换指令。
type GeminiStateReport struct {
	ActiveAccountID int64  `json:"active_account_id"`
	SwitchDone      string `json:"switch_done"`
}

// ReportGeminiState 记录客户端当前用的账号，清掉已执行的切换指令。
func (s *Server) ReportGeminiState(ctx context.Context, deviceID int64, r GeminiStateReport) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO gemini_device_state (device_id, active_account_id, reported_at) VALUES (?, ?, ?)
		ON CONFLICT (device_id) DO UPDATE SET active_account_id = excluded.active_account_id, reported_at = excluded.reported_at`,
		deviceID, r.ActiveAccountID, now); err != nil {
		return err
	}
	if r.SwitchDone != "" {
		res, err := s.DB.ExecContext(ctx, `UPDATE gemini_device_state SET switch_id = '', switch_to = 0
			WHERE device_id = ? AND switch_id = ?`, deviceID, r.SwitchDone)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			s.geminiChanged()
		}
	}
	return nil
}

// RequestGeminiSwitch 让客户端切换到指定账号（管理页面的「切换」）。账号必须分配给它且已有凭据。
func (s *Server) RequestGeminiSwitch(ctx context.Context, deviceID, accountID int64) (GeminiSwitch, error) {
	ok, err := s.assigned(ctx, accountID, deviceID)
	if err != nil {
		return GeminiSwitch{}, err
	}
	if !ok {
		return GeminiSwitch{}, &InputError{"这个账号没有分配给该客户端"}
	}
	a, err := s.geminiAccount(ctx, accountID)
	if err != nil {
		return GeminiSwitch{}, err
	}
	if !a.HasCredential {
		return GeminiSwitch{}, &InputError{"这个账号还没有登录"}
	}
	sw := GeminiSwitch{ID: newLoginID(), AccountID: accountID}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO gemini_device_state (device_id, switch_id, switch_to) VALUES (?, ?, ?)
		ON CONFLICT (device_id) DO UPDATE SET switch_id = excluded.switch_id, switch_to = excluded.switch_to`,
		deviceID, sw.ID, sw.AccountID); err != nil {
		return GeminiSwitch{}, err
	}
	s.geminiChanged()
	return sw, nil
}
