package dist

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseQuotaModels(t *testing.T) {
	// 以模型名为键；remainingFraction 为 0 时被省略。
	got := parseQuotaModels(json.RawMessage(`{
		"gemini-3-pro": {"displayName": "Gemini 3 Pro", "quotaInfo": {"remainingFraction": 0.4, "resetTime": "2026-11-23T15:00:00Z"}},
		"claude-sonnet": {"displayName": "Claude Sonnet", "quotaInfo": {"resetTime": "2026-11-23T16:00:00Z"}},
		"chat-internal": {"displayName": "无额度信息"}
	}`))
	if len(got) != 2 || got[0].Key != "claude-sonnet" || got[0].Remaining != 0 || got[1].Remaining != 0.4 || got[1].Label != "Gemini 3 Pro" {
		t.Fatalf("map form = %+v", got)
	}
	// 数组形式。
	got = parseQuotaModels(json.RawMessage(`[{"model": "m1", "quotaInfo": {"remainingFraction": 1.5}}]`))
	if len(got) != 1 || got[0].Key != "m1" || got[0].Remaining != 1 || got[0].Label != "m1" {
		t.Fatalf("list form = %+v", got)
	}
	if parseProject(json.RawMessage(`"p-1"`)) != "p-1" || parseProject(json.RawMessage(`{"id":"p-2"}`)) != "p-2" {
		t.Fatal("project forms")
	}
}

func TestGeminiQuotaCollect(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	c, d := boundClient(t, s)
	a, _ := s.CreateGeminiAccount(ctx, GeminiAccountInput{Email: "a@gmail.com", Devices: []int64{d.ID}})
	_, _ = s.SetGeminiOAuth(ctx, DefaultGeminiClientID, "secret-x")
	// 凭据里的访问令牌已过期：要先用 refresh token 换新的。
	if _, err := s.UploadGeminiCredential(ctx, d, a.ID, CredentialUpload{Replace: true, Credential: testCredential("a@gmail.com", "ra")}); err != nil {
		t.Fatal(err)
	}
	forms := fakeGeminiGoogle(t, "a@gmail.com")

	var models atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1internal:loadCodeAssist", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ya29.fresh" || !strings.HasPrefix(r.UserAgent(), "antigravity/") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"cloudaicompanionProject": "proj-1"}`))
	})
	mux.HandleFunc("POST /v1internal:fetchAvailableModels", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Project string `json:"project"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Project != "proj-1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		models.Add(1)
		_, _ = w.Write([]byte(`{"models": {"gemini-3-pro": {"displayName": "Gemini 3 Pro", "quotaInfo": {"remainingFraction": 0.25, "resetTime": "2026-11-23T15:00:00Z"}}}}`))
	})
	cc := httptest.NewServer(mux)
	t.Cleanup(cc.Close)
	geminiEndpoints.CloudCode = cc.URL

	if err := c.syncGemini(ctx, 0); err != nil {
		t.Fatal(err)
	}
	c.collectGeminiQuota(ctx)
	st := c.GeminiStatus()
	q := st.Accounts[0].Quota
	if q == nil || q.Error != "" || len(q.Buckets) != 1 || q.Buckets[0].Remaining != 0.25 {
		t.Fatalf("quota = %+v", q)
	}
	if f := (*forms)[0]; f.Get("grant_type") != "refresh_token" || f.Get("refresh_token") != "ra" {
		t.Fatalf("refresh form = %v", f)
	}
	// 访问令牌缓存在内存里：再采一次不再换令牌。
	c.collectGeminiQuota(ctx)
	if len(*forms) != 1 || models.Load() != 2 {
		t.Fatalf("token requests = %d, model requests = %d", len(*forms), models.Load())
	}
	// 管理服务收到了采样。
	var n int
	var rem float64
	_ = s.DB.QueryRow(`SELECT COUNT(*), MIN(remaining) FROM gemini_quota WHERE account_id = ?`, a.ID).Scan(&n, &rem)
	if n != 2 || rem != 0.25 {
		t.Fatalf("server samples = %d %v", n, rem)
	}
	// 没分配到的账号的采样被忽略。
	if err := s.RecordGeminiQuota(ctx, d.ID, []GeminiQuotaSample{{AccountID: 999, Buckets: []GeminiQuotaBucket{{Key: "x", Remaining: 1}}}}); err != nil {
		t.Fatal(err)
	}
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM gemini_quota`).Scan(&n)
	if n != 2 {
		t.Fatalf("unassigned sample stored: %d", n)
	}

	// refresh token 被撤销：显示需要重新登录。
	c.mu.Lock()
	c.gemTokens = nil
	c.mu.Unlock()
	_, _ = s.UploadGeminiCredential(ctx, d, a.ID, CredentialUpload{Replace: true, Credential: testCredential("a@gmail.com", "revoked")})
	_ = c.syncGemini(ctx, 0)
	c.collectGeminiQuota(ctx)
	if q := c.GeminiStatus().Accounts[0].Quota; q == nil || !strings.Contains(q.Error, "重新登录") {
		t.Fatalf("revoked quota = %+v", q)
	}
}

// 写入本机前：存的访问令牌已过期时先刷新，凭据里带上新的 access_token、expiry 和 id_token_email；再取一次得到同一份。
func TestGeminiApplyFresh(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	c, d := boundClient(t, s)
	a, _ := s.CreateGeminiAccount(ctx, GeminiAccountInput{Email: "a@gmail.com", Devices: []int64{d.ID}})
	_, _ = s.SetGeminiOAuth(ctx, DefaultGeminiClientID, "secret-x")
	if _, err := s.UploadGeminiCredential(ctx, d, a.ID, CredentialUpload{Replace: true, Credential: testCredential("a@gmail.com", "ra")}); err != nil {
		t.Fatal(err)
	}
	forms := fakeGeminiGoogle(t, "a@gmail.com")
	if err := c.syncGemini(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.SetGeminiActive(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	ap, err := c.GeminiApplyFresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	info, err := ParseGeminiCredential(ap.Credential)
	if err != nil || info.AccessToken != "ya29.fresh" || info.RefreshToken != "ra" || !info.Expiry.After(time.Now()) {
		t.Fatalf("fresh credential = %+v, %v", info, err)
	}
	var raw struct {
		Email string `json:"id_token_email"`
	}
	_ = json.Unmarshal(ap.Credential, &raw)
	if raw.Email != "a@gmail.com" {
		t.Fatalf("id_token_email = %q", raw.Email)
	}
	again, _ := c.GeminiApplyFresh(ctx)
	if string(again.Credential) != string(ap.Credential) || len(*forms) != 1 {
		t.Fatalf("second call refreshed again (%d requests) or changed credential", len(*forms))
	}
}

// retrieveUserQuotaSummary 的窗口归成固定名称、固定顺序；remainingFraction 省略表示用完；免费账号不显示 5 小时。
func TestParseQuotaSummary(t *testing.T) {
	raw := json.RawMessage(`{"groups": [
		{"displayName": "Gemini", "buckets": [
			{"bucketId": "gemini-weekly", "window": "weekly", "remainingFraction": 0.8, "resetTime": "2026-11-30T00:00:00Z"},
			{"bucketId": "gemini-5h", "window": "5h", "remainingFraction": 0.5}]},
		{"displayName": "Claude", "buckets": [
			{"bucketId": "3p-5h", "remainingFraction": 0.9},
			{"bucketId": "3p-weekly"}]},
		{"displayName": "Gemini 4 Argon", "buckets": [{"bucketId": "gemini-argon-wtu-weekly", "remainingFraction": 1}]},
		{"displayName": "Something New", "buckets": [{"bucketId": "nova-weekly", "remainingFraction": 0.3}]}
	]}`)
	got := parseQuotaSummary(raw, false)
	want := []struct {
		key, label string
		rem        float64
	}{
		{"claude:5h", "Claude（5 小时）", 0.9},
		{"claude:weekly", "Claude（每周）", 0},
		{"gemini:5h", "Gemini（5 小时）", 0.5},
		{"gemini:weekly", "Gemini（每周）", 0.8},
		{"argon:weekly", "Gemini 4 Argon（每周）", 1},
		{"nova-weekly", "Something New（每周）", 0.3},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i, w := range want {
		if got[i].Key != w.key || got[i].Label != w.label || got[i].Remaining != w.rem {
			t.Fatalf("[%d] = %+v, want %+v", i, got[i], w)
		}
	}
	if got[3].ResetAt != "2026-11-30T00:00:00Z" {
		t.Fatalf("reset = %q", got[3].ResetAt)
	}
	for _, b := range parseQuotaSummary(raw, true) {
		if strings.HasSuffix(b.Key, "5h") {
			t.Fatalf("free tier shows %s", b.Key)
		}
	}
	if !isFreeTier("free-tier") || isFreeTier("g1-pro-tier") || isFreeTier("g1-ultra-tier") || isFreeTier("") {
		t.Fatal("isFreeTier")
	}
	if len(parseQuotaSummary(json.RawMessage(`{}`), false)) != 0 {
		t.Fatal("empty summary")
	}
}

// 存的访问令牌看起来没过期、却被 Google 拒绝（401）：用 refresh token 换新的重试一次，之后都用新的。
// 存的到期时间比一小时还远：不可信，直接换新的。
func TestGeminiQuotaStaleAccess(t *testing.T) {
	now := time.Now()
	if storedAccessUsable(GeminiCredentialInfo{AccessToken: "x", Expiry: now.Add(3 * time.Hour)}, now) ||
		!storedAccessUsable(GeminiCredentialInfo{AccessToken: "x", Expiry: now.Add(30 * time.Minute)}, now) ||
		storedAccessUsable(GeminiCredentialInfo{AccessToken: "x", Expiry: now.Add(time.Minute)}, now) {
		t.Fatal("storedAccessUsable")
	}

	ctx := context.Background()
	s := newServer(t)
	c, d := boundClient(t, s)
	a, _ := s.CreateGeminiAccount(ctx, GeminiAccountInput{Email: "a@gmail.com", Devices: []int64{d.ID}})
	_, _ = s.SetGeminiOAuth(ctx, DefaultGeminiClientID, "secret-x")
	cred, _ := json.Marshal(map[string]any{
		"token":       map[string]string{"access_token": "ya29.stale", "token_type": "Bearer", "refresh_token": "ra", "expiry": now.Add(30 * time.Minute).UTC().Format(time.RFC3339Nano)},
		"auth_method": "consumer",
		"id_token":    testIDToken("a@gmail.com"),
	})
	if _, err := s.UploadGeminiCredential(ctx, d, a.ID, CredentialUpload{Replace: true, Credential: cred}); err != nil {
		t.Fatal(err)
	}
	forms := fakeGeminiGoogle(t, "a@gmail.com")

	var rejected, models atomic.Int32
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer ya29.fresh" {
			rejected.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Request had invalid authentication credentials."}}`))
			return false
		}
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1internal:loadCodeAssist", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			_, _ = w.Write([]byte(`{"cloudaicompanionProject": "proj-1", "paidTier": {"id": "g1-pro-tier"}}`))
		}
	})
	mux.HandleFunc("POST /v1internal:retrieveUserQuotaSummary", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			_, _ = w.Write([]byte(`{"groups": [{"buckets": [{"bucketId": "gemini-5h", "remainingFraction": 0.6}, {"bucketId": "3p-weekly", "remainingFraction": 0.2}]}]}`))
		}
	})
	mux.HandleFunc("POST /v1internal:fetchAvailableModels", func(w http.ResponseWriter, r *http.Request) {
		models.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	cc := httptest.NewServer(mux)
	t.Cleanup(cc.Close)
	geminiEndpoints.CloudCode = cc.URL

	if err := c.syncGemini(ctx, 0); err != nil {
		t.Fatal(err)
	}
	c.collectGeminiQuota(ctx)
	q := c.GeminiStatus().Accounts[0].Quota
	if q == nil || q.Error != "" || len(q.Buckets) != 2 || q.Buckets[0].Key != "claude:weekly" || q.Buckets[1].Label != "Gemini（5 小时）" {
		t.Fatalf("quota = %+v", q)
	}
	if rejected.Load() != 1 || len(*forms) != 1 || models.Load() != 0 {
		t.Fatalf("rejected = %d, refreshes = %d, models = %d", rejected.Load(), len(*forms), models.Load())
	}
	// 再采一次：用内存里的新令牌，不再碰失效的那个，也不再刷新。
	c.collectGeminiQuota(ctx)
	if rejected.Load() != 1 || len(*forms) != 1 {
		t.Fatalf("second round: rejected = %d, refreshes = %d", rejected.Load(), len(*forms))
	}
	// 写进本机的凭据也带新令牌。
	if err := c.SetGeminiActive(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	ap, err := c.GeminiApplyFresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := ParseGeminiCredential(ap.Credential); info.AccessToken != "ya29.fresh" {
		t.Fatalf("applied access token = %q", info.AccessToken)
	}
}
