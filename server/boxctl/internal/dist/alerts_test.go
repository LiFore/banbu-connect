package dist

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"boxctl/internal/subscriptions"
)

// barkSink 起一个假的 Bark 服务器，记下收到的标题。
func barkSink(t *testing.T, s *Server) func() []string {
	t.Helper()
	var mu sync.Mutex
	titles := []string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		titles = append(titles, m["title"].(string))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"code":200}`))
	}))
	t.Cleanup(ts.Close)
	keys := []string{"https://api.day.app/abcdKEY/标题"} // 粘贴推送地址时取出 key
	if err := s.SaveNotify(context.Background(), NotifyInput{BarkEnabled: true, BarkServer: ts.URL, BarkKeys: &keys}); err != nil {
		t.Fatal(err)
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := titles
		titles = []string{}
		return out
	}
}

func TestNotifyConfigKeepsSecrets(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	keys := []string{"https://api.day.app/abcdKEY/标题"}
	hook := "https://open.feishu.cn/open-apis/bot/v2/hook/x"
	if err := s.SaveNotify(ctx, NotifyInput{BarkEnabled: true, BarkKeys: &keys, FeishuHook: &hook}); err != nil {
		t.Fatal(err)
	}
	// 敏感项不传表示不改。
	if err := s.SaveNotify(ctx, NotifyInput{BarkEnabled: true, FeishuEnabled: true}); err != nil {
		t.Fatal(err)
	}
	v, _ := s.Notify(ctx)
	if len(v.BarkKeysMasked) != 1 || v.BarkKeysMasked[0] != "••••dKEY" || !v.FeishuHookSet || v.FeishuSecretSet {
		t.Fatalf("配置不对：%+v", v)
	}
	var raw notifySetting
	_ = s.loadSetting(ctx, notifyKey, &raw)
	b, _ := json.Marshal(raw)
	if strings.Contains(string(b), "abcdKEY") || strings.Contains(string(b), "feishu.cn") {
		t.Fatal("key 和 Webhook 应加密保存")
	}
	empty := []string{}
	if err := s.SaveNotify(ctx, NotifyInput{BarkEnabled: true, BarkKeys: &empty}); err == nil {
		t.Fatal("启用 Bark 却没有 key 应失败")
	}
}

func TestTrafficAlertOncePerPeriod(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	sent := barkSink(t, s)
	if _, err := s.SaveAlertRules(ctx, AlertRules{Timezone: "Asia/Shanghai", Traffic: []TrafficRule{
		{Name: "US ISP", Period: "month", LimitGB: 1, Enabled: true},
		{Name: "关闭的", Period: "day", LimitGB: 0.001, Enabled: false},
	}}); err != nil {
		t.Fatal(err)
	}
	hour := time.Now().UTC().Truncate(time.Hour).Format(time.RFC3339)
	_, _ = s.DB.Exec(`INSERT INTO traffic_hours (hour, name, upload, download) VALUES (?, 'US ISP', ?, ?), (?, '关闭的', 9, 9)`, hour, 1<<29, 1<<28, hour)
	if n, err := s.CheckAlerts(ctx); err != nil || n != 0 {
		t.Fatalf("未超限不应提醒：%d %v", n, err)
	}
	_, _ = s.DB.Exec(`UPDATE traffic_hours SET download = ? WHERE name = 'US ISP'`, 1<<29)
	if n, _ := s.CheckAlerts(ctx); n != 1 {
		t.Fatalf("超限应提醒一次：%d", n)
	}
	if n, _ := s.CheckAlerts(ctx); n != 0 {
		t.Fatal("同一周期不应重复提醒")
	}
	if got := sent(); len(got) != 1 || got[0] != "节点流量超限：US ISP" {
		t.Fatalf("推送不对：%v", got)
	}
	// 提高上限后重新判断。
	r, _ := s.AlertRules(ctx)
	r.Traffic[0].LimitGB = 0.5
	_, _ = s.SaveAlertRules(ctx, r)
	if n, _ := s.CheckAlerts(ctx); n != 1 {
		t.Fatal("改了上限应重新判断")
	}
	if log, _ := s.NotifyLog(ctx); len(log) != 2 || log[0].Result != "" || log[0].Kind != "node_traffic" {
		t.Fatalf("推送记录不对：%+v", log)
	}
}

func TestSubscriptionAndAIAlerts(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	s.Subs = subscriptions.New(s.DB, s.Secrets, "")
	sent := barkSink(t, s)
	exp := time.Now().Add(48 * time.Hour).Unix()
	_, _ = s.DB.Exec(`INSERT INTO subscriptions (id, name, userinfo) VALUES ('s1', '机场', ?)`,
		`{"upload":0,"download":950,"total":1000,"expire":`+itoa(exp)+`}`)
	a, err := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	q, _ := json.Marshal(AIQuota{Windows: []AIQuotaWindow{{Key: "five_hour", Label: "5 小时", UsedPercent: 95, ResetAt: "2026-11-23T10:00:00Z"},
		{Key: "seven_day", Label: "7 天", UsedPercent: 40}}, CheckedAt: time.Now()})
	_, _ = s.DB.Exec(`UPDATE claude_accounts SET quota = ? WHERE id = ?`, string(q), a.ID)

	if _, err := s.CheckAlerts(ctx); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(sent(), "|")
	for _, want := range []string{"订阅流量已用 95%：机场", "订阅即将到期：机场", "Claude 额度已用 95%：a@example.com"} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺少提醒 %q：%s", want, got)
		}
	}
	if strings.Contains(got, "7 天") {
		t.Fatal("未超过阈值的窗口不应提醒")
	}
	// 订阅重置流量后再超过，重新提醒。
	_, _ = s.DB.Exec(`UPDATE subscriptions SET userinfo = '{"download":10,"total":1000}' WHERE id = 's1'`)
	_, _ = s.CheckAlerts(ctx)
	_, _ = s.DB.Exec(`UPDATE subscriptions SET userinfo = '{"download":990,"total":1000}' WHERE id = 's1'`)
	_, _ = s.CheckAlerts(ctx)
	if got := sent(); len(got) != 1 || !strings.HasPrefix(got[0], "订阅流量已用 99%") {
		t.Fatalf("恢复后再超过应重新提醒：%v", got)
	}
}

func TestSubscriptionAutoUpdatePublishes(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	sent := barkSink(t, s)
	seed(t, s)
	sb := subscriptions.Subscription{ID: "s1", Name: "机场"}
	// 失败：同一原因只推一次。
	s.OnSubscriptionUpdate(sb, subscriptions.Diff{}, errors.New("HTTP 403"))
	s.OnSubscriptionUpdate(sb, subscriptions.Diff{}, errors.New("HTTP 403"))
	s.OnSubscriptionUpdate(sb, subscriptions.Diff{}, subscriptions.ErrNeedsConfirm)
	if got := sent(); len(got) != 2 || got[0] != "订阅自动更新失败：机场" || got[1] != "订阅需要手动确认更新：机场" {
		t.Fatalf("失败推送不对：%v", got)
	}
	// 成功且有变化：自动发布并推送摘要。
	s.OnSubscriptionUpdate(sb, subscriptions.Diff{Added: []subscriptions.NodeRef{{DisplayName: "HK 01"}}}, nil)
	s.AfterSubscriptionsUpdated()
	cur, ok, _ := s.Current(ctx)
	if !ok || cur.Note != "订阅自动更新" {
		t.Fatalf("应自动发布：%+v", cur)
	}
	if got := sent(); len(got) != 1 || got[0] != "订阅已更新并发布 v1" {
		t.Fatalf("发布推送不对：%v", got)
	}
	// 下发内容没变时不再发布新版本。
	s.OnSubscriptionUpdate(sb, subscriptions.Diff{Removed: []subscriptions.NodeRef{{DisplayName: "旧"}}}, nil)
	s.AfterSubscriptionsUpdated()
	if cur, _, _ := s.Current(ctx); cur.Version != 1 {
		t.Fatalf("内容没变不应发布：v%d", cur.Version)
	}
	if got := sent(); len(got) != 1 || got[0] != "订阅已更新，下发内容没有变化" {
		t.Fatalf("推送不对：%v", got)
	}
}
