package dist

import (
	"context"
	"testing"
	"time"
)

func TestAIOnlineSessions(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	d1, d2 := newDevice(t, s), newDevice(t, s)
	a, err := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Kind: KindClaude, Email: "a@example.com", Devices: []int64{d1.ID, d2.ID}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Kind: KindCodex, Email: "b@example.com", Devices: []int64{d1.ID}})
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGeminiAccount(ctx, GeminiAccountInput{Email: "g@gmail.com", MaxDevices: 2, Devices: []int64{d1.ID}})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 24, 23, 30, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	report := func(d Device, m int, acc ...AIOnlineAccount) {
		t.Helper()
		if err := s.RecordAIOnline(ctx, d.ID, acc, at(m)); err != nil {
			t.Fatal(err)
		}
	}
	A := AIOnlineAccount{KindClaude, a.ID}
	B := AIOnlineAccount{KindCodex, b.ID}
	G := AIOnlineAccount{KindGemini, g.ID}

	// d1：A 从 0 到 40 分钟（切到 B），B 从 40 到 50，之后离线（最后一次上报 50 分钟），70 分钟重新上线用 B 到 80。
	// Gemini 一直是 G。不存在的账号和类型不对的账号忽略。
	for m := 0; m < 40; m++ {
		report(d1, m, A, G, AIOnlineAccount{KindGemini, 999}, AIOnlineAccount{KindCodex, a.ID})
	}
	for m := 40; m <= 50; m++ {
		report(d1, m, B, G)
	}
	if err := s.CloseStaleAIOnline(ctx, at(60)); err != nil {
		t.Fatal(err)
	}
	for m := 70; m <= 80; m++ {
		report(d1, m, B, G)
	}
	report(d1, 81) // 都切走了
	// d2：A 从 10 到 20。
	for m := 10; m <= 20; m++ {
		report(d2, m, A)
	}
	report(d2, 20)

	st, err := s.AIOnlineStatsFor(ctx, at(-60), at(120), time.UTC, AIOnlineFilter{}, at(120))
	if err != nil {
		t.Fatal(err)
	}
	got := map[[2]int64]map[string]int64{}
	for _, tot := range st.Totals {
		k := [2]int64{tot.DeviceID, tot.AccountID}
		if got[k] == nil {
			got[k] = map[string]int64{}
		}
		got[k][tot.Kind] = tot.Seconds / 60
		if tot.Kind == KindCodex && tot.Sessions != 2 {
			t.Fatalf("codex sessions = %d, want 2", tot.Sessions)
		}
	}
	want := map[[2]int64]map[string]int64{
		{d1.ID, a.ID}: {KindClaude: 40},
		{d1.ID, b.ID}: {KindCodex: 10 + 11},
		{d1.ID, g.ID}: {KindGemini: 50 + 11},
		{d2.ID, a.ID}: {KindClaude: 10},
	}
	for k, w := range want {
		for kind, m := range w {
			if got[k][kind] != m {
				t.Fatalf("device %d account %d %s = %d min, want %d (all: %v)", k[0], k[1], kind, got[k][kind], m, got)
			}
		}
	}
	if len(st.Totals) != 4 {
		t.Fatalf("totals = %+v", st.Totals)
	}

	// 按天切分：t0 是 23:30，A 在 d1 上 30 分钟在 24 日、10 分钟在 25 日；d2 的 10 分钟（23:40~23:50）在 24 日。
	days := map[string]int64{}
	for _, d := range st.Days {
		if d.Kind == KindClaude {
			days[d.Day] += d.Seconds / 60
		}
	}
	if days["2026-11-24"] != 40 || days["2026-11-25"] != 10 {
		t.Fatalf("days = %v", days)
	}

	// 范围截断：只看 0~20 分钟，A 在 d1 上 20 分钟。
	st, _ = s.AIOnlineStatsFor(ctx, at(0), at(20), time.UTC, AIOnlineFilter{DeviceID: d1.ID, Kind: KindClaude}, at(120))
	if len(st.Totals) != 1 || st.Totals[0].Seconds != 20*60 {
		t.Fatalf("clipped = %+v", st.Totals)
	}

	// 进行中：d2 现在用 A，卡片上显示在线。
	for m := 100; m <= 105; m++ {
		report(d2, m, A)
	}
	sums, err := s.AIOnlineSummaries(ctx, dayStart(at(105), time.UTC), at(106))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, sum := range sums {
		if sum.Kind != KindClaude || sum.AccountID != a.ID {
			continue
		}
		found = true
		// 今天（25 日 0 点起）：d1 10 分钟，d2 现在这段 6 分钟（之前的 10 分钟在 24 日）。
		if sum.Today != (10+6)*60 || sum.Week != (40+10+6)*60 {
			t.Fatalf("summary = %+v", sum)
		}
		if len(sum.Devices) != 2 || sum.Devices[0].DeviceID != d2.ID || sum.Devices[0].OnlineSince == "" {
			t.Fatalf("devices = %+v", sum.Devices)
		}
	}
	if !found {
		t.Fatalf("summaries = %+v", sums)
	}
}
