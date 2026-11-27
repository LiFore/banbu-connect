package dist

import (
	"context"
	"errors"
	"testing"
	"time"
)

func syncAccount(t *testing.T, s *Server, deviceID, accountID int64) (AISyncAccount, []AIRefreshJob) {
	t.Helper()
	out, err := s.AISync(context.Background(), deviceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range out.Accounts {
		if a.ID == accountID {
			return a, out.Jobs
		}
	}
	t.Fatalf("device %d has no account %d: %+v", deviceID, accountID, out)
	return AISyncAccount{}, nil
}

// keepOnline 让客户端在测试里模拟的时间范围内都算在线（模拟时间比真实时间快）。
func keepOnline(s *Server, ids ...int64) {
	for _, id := range ids {
		s.noteRefreshCap(id)
		s.refreshMu.Lock()
		s.refresh.caps[id] = time.Now().Add(24 * time.Hour)
		s.refreshMu.Unlock()
	}
}

func TestClaudeRefreshFlow(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	d1, d2 := newDevice(t, s), newDevice(t, s)
	now := time.Now()
	exp := now.Add(10 * time.Minute).UnixMilli() // 已进入续期窗口（过期前 30 分钟）
	a := must(s.CreateClaudeAccount(ctx, ClaudeAccountInput{Email: "a@example.com", Devices: []int64{d1.ID, d2.ID},
		AccessToken: `{"claudeAiOauth":{"accessToken":"at-1","refreshToken":"rt-1","expiresAt":` + itoa(exp) + `}}`}))
	if a.CredExpires != exp {
		t.Fatalf("cred_expires = %d", a.CredExpires)
	}

	// 客户端只拿 Access Token。
	sa, _ := syncAccount(t, s, d1.ID, a.ID)
	if !sa.TokenOnly || sa.Credential == nil || sa.Credential.AccessToken != "at-1" || sa.Credential.RefreshToken != "" {
		t.Fatalf("d1 sync = %+v", sa)
	}

	// 没有在线的新版客户端：等待。
	if err := s.stepAIRefresh(ctx, now); err != nil {
		t.Fatal(err)
	}
	st := s.refreshState(a.ID)
	if st == nil || st.Phase != RefreshNotice {
		t.Fatalf("state = %+v", st)
	}
	_ = s.stepAIRefresh(ctx, now.Add(refreshNotice+time.Second))
	if st = s.refreshState(a.ID); st.Phase != RefreshWaiting {
		t.Fatalf("no online client: state = %+v", st)
	}

	// 两台都在线：重新发起，先通知。
	keepOnline(s, d1.ID, d2.ID)
	_ = s.stepAIRefresh(ctx, now.Add(3*time.Minute))
	st = s.refreshState(a.ID)
	if st.Phase != RefreshNotice {
		t.Fatalf("retry should notice again: %+v", st)
	}
	for _, d := range []int64{d1.ID, d2.ID} {
		sa, jobs := syncAccount(t, s, d, a.ID)
		if sa.Refresh == nil || sa.Refresh.Phase != RefreshNotice || len(jobs) != 0 {
			t.Fatalf("device %d should see notice only: %+v %+v", d, sa.Refresh, jobs)
		}
	}
	// d2 正在用：宽限期内不续期；d2 退出后两台都空闲，提前续期，优先交给空闲的。
	_ = s.AckAIRefresh(ctx, d1, st.ID, false)
	_ = s.AckAIRefresh(ctx, d2, st.ID, true)
	_ = s.stepAIRefresh(ctx, now.Add(3*time.Minute+time.Second))
	if s.refreshState(a.ID).Phase != RefreshNotice {
		t.Fatal("busy client should hold the notice")
	}
	_ = s.AckAIRefresh(ctx, d2, st.ID, false)
	_ = s.stepAIRefresh(ctx, now.Add(3*time.Minute+2*time.Second))
	st = s.refreshState(a.ID)
	if st.Phase != RefreshRunning || st.DeviceID == 0 {
		t.Fatalf("should dispatch: %+v", st)
	}
	exec, other := d1, d2
	if st.DeviceID == d2.ID {
		exec, other = d2, d1
	}
	_, jobs := syncAccount(t, s, exec.ID, a.ID)
	if len(jobs) != 1 || jobs[0].Credential.RefreshToken != "rt-1" || jobs[0].BaseVersion != a.CredVersion {
		t.Fatalf("executor job = %+v", jobs)
	}
	if _, jobs := syncAccount(t, s, other.ID, a.ID); len(jobs) != 0 {
		t.Fatal("only the executor gets the refresh token")
	}

	// 交回新令牌：保存、版本加 1，所有客户端收到新的 Access Token。
	newExp := now.Add(8 * time.Hour).UnixMilli()
	err := s.FinishAIRefresh(ctx, exec, jobs[0].ID, AIRefreshResult{AccountID: a.ID, BaseVersion: jobs[0].BaseVersion, OK: true,
		Credential: &AICredential{AccessToken: "at-2", RefreshToken: "rt-2", ExpiresAt: newExp}})
	if err != nil {
		t.Fatal(err)
	}
	a = must(s.claudeAccount(ctx, a.ID))
	if a.Refresh != nil || a.CredVersion != 2 || a.RefreshAt == "" || a.CredExpires != newExp {
		t.Fatalf("after refresh = %+v", a)
	}
	sa, _ = syncAccount(t, s, other.ID, a.ID)
	if sa.Credential.AccessToken != "at-2" || sa.Credential.RefreshToken != "" || sa.Refresh != nil {
		t.Fatalf("other sync = %+v", sa)
	}
	_ = s.stepAIRefresh(ctx, now.Add(4*time.Minute))
	if s.refreshState(a.ID) != nil {
		t.Fatal("fresh token should not refresh again")
	}

	// 重复交回同一个任务的结果（版本已变）：冲突，不覆盖新凭据。
	err = s.FinishAIRefresh(ctx, exec, jobs[0].ID, AIRefreshResult{AccountID: a.ID, BaseVersion: jobs[0].BaseVersion, OK: true,
		Credential: &AICredential{AccessToken: "at-x", RefreshToken: "rt-x"}})
	if !errors.Is(err, ErrCredentialConflict) {
		t.Fatalf("stale result err = %v", err)
	}

	// 立即续期 → Refresh Token 被拒绝：标为永久失败，不再自动重试。
	s.refresh.done[a.ID] = time.Time{}
	if _, err := s.StartAIRefresh(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(refreshNotice + time.Second)
	_ = s.stepAIRefresh(ctx, later)
	st = s.refreshState(a.ID)
	if st.Phase != RefreshRunning {
		t.Fatalf("manual refresh should dispatch after notice: %+v", st)
	}
	execDev := d1
	if st.DeviceID == d2.ID {
		execDev = d2
	}
	_ = s.FinishAIRefresh(ctx, execDev, st.ID, AIRefreshResult{AccountID: a.ID, BaseVersion: 2, Error: "invalid_grant", Permanent: true})
	_ = s.stepAIRefresh(ctx, later.Add(time.Hour))
	if st = s.refreshState(a.ID); st == nil || st.Phase != RefreshFailed || !st.Permanent {
		t.Fatalf("permanent failure = %+v", st)
	}
	if a = must(s.claudeAccount(ctx, a.ID)); a.RefreshError != "invalid_grant" {
		t.Fatalf("refresh_error = %q", a.RefreshError)
	}
	// 管理员重新填入令牌：失败状态作废，错误清空。
	a = must(s.SetClaudeCredential(ctx, a.ID, "", "rt-new"))
	_ = s.stepAIRefresh(ctx, later.Add(time.Hour))
	if st = s.refreshState(a.ID); st == nil || st.Phase != RefreshNotice || a.RefreshError != "" {
		t.Fatalf("new credential should start over: %+v %q", st, a.RefreshError)
	}
}

func TestCodexHolder(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	d1, d2 := newDevice(t, s), newDevice(t, s)
	// 访问令牌 10 天有效、还剩 2 天：进入续期窗口（3 天）。
	iat := time.Now().Add(-8 * 24 * time.Hour).Unix()
	access := codexJWT(`{"iat":` + itoa(iat) + `,"exp":` + itoa(iat+10*86400) + `}`)
	id := codexJWT(`{"email":"c@example.com"}`)
	a := must(s.CreateClaudeAccount(ctx, ClaudeAccountInput{Kind: KindCodex, Devices: []int64{d1.ID, d2.ID}, HolderDevice: d2.ID,
		AccessToken: `{"tokens":{"access_token":"` + access + `","refresh_token":"rt-c","id_token":"` + id + `"}}`}))
	if a.Holder != d2.ID {
		t.Fatalf("holder = %d", a.Holder)
	}
	if sa, _ := syncAccount(t, s, d2.ID, a.ID); sa.TokenOnly || sa.Credential.RefreshToken != "rt-c" {
		t.Fatalf("holder should get full credential: %+v", sa)
	}
	if sa, _ := syncAccount(t, s, d1.ID, a.ID); !sa.TokenOnly || sa.Credential.RefreshToken != "" || sa.Credential.IDToken == "" {
		t.Fatalf("non-holder should get access + id token only: %+v", sa)
	}

	// Codex 不通知，直接交给持有者。
	s.noteRefreshCap(d1.ID)
	s.noteRefreshCap(d2.ID)
	_ = s.stepAIRefresh(ctx, time.Now())
	if st := s.refreshState(a.ID); st == nil || st.Phase != RefreshRunning || st.DeviceID != d2.ID {
		t.Fatalf("codex refresh should go to holder: %+v", st)
	}

	// 持有者必须是分配到的客户端；不在列表里时退回第一台。
	a = must(s.UpdateClaudeAccount(ctx, a.ID, ClaudeAccountInput{Email: a.Email, Devices: []int64{d1.ID}, HolderDevice: d2.ID}))
	if a.HolderDevice != 0 || a.Holder != d1.ID {
		t.Fatalf("holder after unassign = %d / %d", a.HolderDevice, a.Holder)
	}
}
