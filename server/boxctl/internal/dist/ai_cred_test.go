package dist

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

func codexJWT(payload string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(payload)) + ".sig"
}

func TestParseAICredentialInput(t *testing.T) {
	// 两栏各填一个令牌。
	c, err := ParseAICredentialInput(KindClaude, " sk-ant-oat01-a ", "sk-ant-ort01-b")
	if err != nil || c.AccessToken != "sk-ant-oat01-a" || c.RefreshToken != "sk-ant-ort01-b" {
		t.Fatalf("c = %+v, err = %v", c, err)
	}
	if _, err := ParseAICredentialInput(KindClaude, "", " "); err == nil {
		t.Fatal("empty should fail")
	}
	// Claude 的凭据 JSON。
	c, err = ParseAICredentialInput(KindClaude, `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":123}}`, "")
	if err != nil || c.AccessToken != "a" || c.RefreshToken != "r" || c.ExpiresAt != 123 {
		t.Fatalf("claude json = %+v, err = %v", c, err)
	}
	// Codex 的 auth.json：从 access token 读出 account id 和过期时间，从 id_token 读出邮箱。
	access := codexJWT(`{"exp":1800000000,"https://api.openai.com/auth":{"chatgpt_account_id":"acc-1"}}`)
	id := codexJWT(`{"email":"X@gmail.com"}`)
	c, err = ParseAICredentialInput(KindCodex, "", `{"auth_mode":"chatgpt","tokens":{"access_token":"`+access+`","refresh_token":"rt","id_token":"`+id+`"}}`)
	if err != nil || c.AccountID != "acc-1" || c.ExpiresAt != 1800000000000 || c.Email() != "x@gmail.com" {
		t.Fatalf("codex json = %+v, err = %v", c, err)
	}
}

func TestAICredentialFlow(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	d1, d2 := newDevice(t, s), newDevice(t, s)
	a, err := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Kind: KindCodex, Email: "x@gmail.com", Devices: []int64{d1.ID}})
	if err != nil {
		t.Fatal(err)
	}

	// 邮箱不对的凭据被拒绝。
	other := codexJWT(`{"email":"y@gmail.com"}`)
	if _, err := s.SetClaudeCredential(ctx, a.ID, `{"tokens":{"refresh_token":"r","id_token":"`+other+`"}}`, ""); err == nil {
		t.Fatal("wrong email should fail")
	}
	a, err = s.SetClaudeCredential(ctx, a.ID, "", "rt-1")
	if err != nil || !a.HasCredential || a.CredVersion != 1 || a.CredSource != "管理员填写" {
		t.Fatalf("a = %+v, err = %v", a, err)
	}

	// 只下发给允许的客户端。
	sync1, err := s.AISync(ctx, d1.ID)
	if err != nil || len(sync1.Accounts) != 1 || sync1.Accounts[0].Credential == nil || sync1.Accounts[0].Credential.RefreshToken != "rt-1" {
		t.Fatalf("sync1 = %+v, err = %v", sync1, err)
	}
	if sync2, _ := s.AISync(ctx, d2.ID); len(sync2.Accounts) != 0 {
		t.Fatalf("d2 should get nothing: %+v", sync2)
	}

	// 客户端上传：版本号过期时冲突，最新时版本号加 1；不允许的客户端看不到账号。
	up := AICredentialUpload{BaseVersion: 0, Credential: AICredential{RefreshToken: "rt-2"}}
	if _, err := s.UploadClaudeCredential(ctx, d1, a.ID, up); !errors.Is(err, ErrCredentialConflict) {
		t.Fatalf("stale upload err = %v", err)
	}
	up.BaseVersion = 1
	if v, err := s.UploadClaudeCredential(ctx, d1, a.ID, up); err != nil || v != 2 {
		t.Fatalf("upload v = %d, err = %v", v, err)
	}
	if _, err := s.UploadClaudeCredential(ctx, d2, a.ID, up); !errors.Is(err, ErrNotFound) {
		t.Fatalf("d2 upload err = %v", err)
	}

	// 额度：只接受允许的客户端。
	q := AIQuota{Windows: []AIQuotaWindow{{Key: "primary", Label: "5 小时", UsedPercent: 12}}, CheckedAt: time.Now()}
	if err := s.RecordAIQuota(ctx, d2, []AIQuotaSample{{AccountID: a.ID, Quota: q}}); err != nil {
		t.Fatal(err)
	}
	if a, _ = s.claudeAccount(ctx, a.ID); a.Quota != nil {
		t.Fatal("d2 quota should be ignored")
	}
	if err := s.RecordAIQuota(ctx, d1, []AIQuotaSample{{AccountID: a.ID, Quota: q}}); err != nil {
		t.Fatal(err)
	}
	if a, _ = s.claudeAccount(ctx, a.ID); a.Quota == nil || a.Quota.Windows[0].UsedPercent != 12 || a.QuotaDevice != d1.Name {
		t.Fatalf("quota = %+v", a.Quota)
	}

	// 换邮箱清掉凭据和额度，版本号继续递增。
	a, err = s.UpdateClaudeAccount(ctx, a.ID, ClaudeAccountInput{Kind: KindCodex, Email: "z@gmail.com", Devices: []int64{d1.ID}})
	if err != nil || a.HasCredential || a.Quota != nil || a.CredVersion != 3 {
		t.Fatalf("after email change = %+v, err = %v", a, err)
	}
}

func TestAuthTest(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	d1 := newDevice(t, s)
	a, err := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Email: "c@gmail.com", Devices: []int64{d1.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAuthTest(ctx, KindClaude, a.ID); err == nil {
		t.Fatal("no credential should fail")
	}
	if _, err := s.SetClaudeCredential(ctx, a.ID, "at", "rt"); err != nil {
		t.Fatal(err)
	}
	// 客户端从没联系过管理服务：不在线。
	if _, err := s.StartAuthTest(ctx, KindClaude, a.ID); err == nil {
		t.Fatal("offline device should fail")
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE devices SET last_seen = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), d1.ID); err != nil {
		t.Fatal(err)
	}
	test, err := s.StartAuthTest(ctx, KindClaude, a.ID)
	if err != nil || test.DeviceID != d1.ID || test.Status != TestPending {
		t.Fatalf("test = %+v, err = %v", test, err)
	}
	sync, _ := s.AISync(ctx, d1.ID)
	if len(sync.Tests) != 1 || sync.Tests[0].ID != test.ID {
		t.Fatalf("tests = %+v", sync.Tests)
	}
	q := &AIQuota{Windows: []AIQuotaWindow{{Key: "five_hour", Label: "5 小时", UsedPercent: 30}}}
	if err := s.FinishAuthTest(ctx, d1, test.ID, AuthTestResult{OK: true, Quota: q}); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ClaudeAccounts(ctx)
	if list[0].AuthTest == nil || list[0].AuthTest.Status != TestOK || list[0].Quota == nil {
		t.Fatalf("account = %+v", list[0])
	}
	if sync, _ := s.AISync(ctx, d1.ID); len(sync.Tests) != 0 {
		t.Fatal("finished test should not be resent")
	}
}
