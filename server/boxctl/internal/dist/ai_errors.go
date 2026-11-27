package dist

import (
	"context"
	"strconv"
)

func acctKey(id int64) string { return "acct:" + strconv.FormatInt(id, 10) }

// ClearedErrors 是一键清除错误的结果：各类错误清掉了多少个账号的。
type ClearedErrors struct {
	Login   int `json:"login"`   // 登录失败（Claude / Codex 的 last_error）
	Refresh int `json:"refresh"` // 续期失败（记录的原因和内存里的失败状态）
	Auth    int `json:"auth"`    // 鉴权测试失败的结果
	Total   int `json:"total"`   // 有错误被清除的账号数（同一个账号的多类错误只算一次）
}

// ClearAIErrors 一键清除所有 AI 账号上显示的错误：登录失败、续期失败的原因和状态、鉴权测试失败的结果。
// 只清掉「错误的记录」，不动凭据、分配和额度；真正的问题还在时，下一次登录、续期或测试会重新报出来。
func (s *Server) ClearAIErrors(ctx context.Context) (ClearedErrors, error) {
	var out ClearedErrors
	touched := map[string]bool{} // acct:id（Claude / Codex 共用 id）或 gemini:id

	rows, err := s.DB.QueryContext(ctx, `SELECT id, kind, last_error != '', refresh_error != '' FROM claude_accounts WHERE last_error != '' OR refresh_error != ''`)
	if err != nil {
		return out, err
	}
	type row struct {
		id           int64
		kind         string
		login, renew bool
	}
	var found []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.kind, &r.login, &r.renew); err != nil {
			rows.Close()
			return out, err
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	if len(found) > 0 {
		if _, err := s.DB.ExecContext(ctx, `UPDATE claude_accounts SET last_error = '', refresh_error = '' WHERE last_error != '' OR refresh_error != ''`); err != nil {
			return out, err
		}
	}
	for _, r := range found {
		if r.login {
			out.Login++
		}
		if r.renew {
			out.Refresh++
		}
		touched[acctKey(r.id)] = true
	}

	// 内存里的续期失败状态（含 Refresh Token 已失效的）。
	s.refreshMu.Lock()
	for id, st := range s.refresh.states {
		if st != nil && st.Phase == RefreshFailed {
			delete(s.refresh.states, id)
			out.Refresh++
			touched[acctKey(id)] = true
		}
	}
	s.refreshMu.Unlock()

	// 鉴权测试失败的结果。
	s.testMu.Lock()
	for k, t := range s.authTests {
		if t.Status == TestFailed {
			delete(s.authTests, k)
			out.Auth++
			if t.Kind == KindGemini {
				touched["gemini:"+strconv.FormatInt(t.AccountID, 10)] = true
			} else {
				touched[acctKey(t.AccountID)] = true
			}
		}
	}
	s.testMu.Unlock()

	out.Total = len(touched)
	return out, nil
}
