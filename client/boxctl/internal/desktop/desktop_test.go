package desktop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"

	"boxctl/internal/core"
	"boxctl/internal/groups"
	"boxctl/internal/killswitch"
	"boxctl/internal/panel"
)

func TestSeedGroupsPassStrictCheckAndRunOnce(t *testing.T) {
	ctx := context.Background()
	svc, err := panel.OpenServices(ctx, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer svc.DB.Close()
	if err := seedGroups(ctx, svc.DB, svc.Groups); err != nil {
		t.Fatal(err)
	}
	list, err := svc.Groups.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]groups.Export{}
	for _, g := range list {
		byName[g.Name] = g
	}
	for _, name := range []string{"Claude", "ChatGPT"} {
		g, ok := byName[name]
		if !ok || !g.Strict {
			t.Fatalf("%s 应是严格组：%+v", name, g)
		}
	}
	// 默认受保护的服务都能对应到严格组。
	cfgEngine, err := killswitch.NewEngine(filepath.Join(t.TempDir(), "k.json"), &killswitch.FakeBackend{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range cfgEngine.Config().Services {
		if !byName[s.Group].Strict {
			t.Fatalf("服务 %s 的组 %s 不是严格组", s.Name, s.Group)
		}
	}
	if g := byName["漏网之鱼"]; len(g.Members) == 0 || g.Members[0] != "DIRECT" {
		t.Fatalf("未知流量应默认直连：%+v", g)
	}

	// 用户删掉默认组后，重启不会再写回来。
	before := len(list)
	if err := seedGroups(ctx, svc.DB, svc.Groups); err != nil {
		t.Fatal(err)
	}
	list, _ = svc.Groups.Export(ctx)
	if len(list) != before {
		t.Fatalf("不应重复写入：%d → %d", before, len(list))
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc, err := panel.OpenServices(ctx, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer svc.DB.Close()
	s, err := loadSettings(ctx, svc.DB)
	if err != nil {
		t.Fatal(err)
	}
	if s.Running || !s.TUN || s.Mode != "rule" || s.Onboarded {
		t.Fatalf("默认值不对：%+v", s)
	}
	s.Mode, s.Running, s.CustomRules = "global", true, []string{"a"}
	if err := saveSettings(ctx, svc.DB, s); err != nil {
		t.Fatal(err)
	}
	if err := saveSettings(ctx, svc.DB, s); err != nil {
		t.Fatal("重复保存不应失败：", err)
	}
	got, err := loadSettings(ctx, svc.DB)
	if err != nil || got.Mode != "global" || !got.Running || !slices.Equal(got.CustomRules, []string{"a"}) {
		t.Fatalf("%+v %v", got, err)
	}
	if got.Onboarded {
		t.Fatal("保存过 onboarded=false 的应保持 false")
	}
	// 引入 onboarded 之前保存的设置没有这个字段，按已完成引导处理。
	if _, err := svc.DB.ExecContext(ctx, `UPDATE settings SET value = '{"mode":"rule"}' WHERE key = ?`, settingsKey); err != nil {
		t.Fatal(err)
	}
	if got, err := loadSettings(ctx, svc.DB); err != nil || !got.Onboarded {
		t.Fatalf("旧设置应视为已完成引导：%+v %v", got, err)
	}
}

func TestRequireLocalHost(t *testing.T) {
	h := requireLocalHost(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	for host, want := range map[string]int{
		"127.0.0.1:17930":        204,
		"localhost:17930":        204,
		"evil.example.com":       403,
		"evil.example.com:17930": 403,
		"127.0.0.1.evil.com:80":  403,
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s：%d", host, w.Code)
		}
	}
}

func TestAutoApplyTriggersOnlyOnSuccessfulWrites(t *testing.T) {
	s := core.NewTestSupervisor()
	status := 200
	h := autoApply(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }), s)
	call := func(method, path string) bool {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
		return s.Triggered()
	}
	for _, p := range []string{"/api/v1/nodes/x", "/api/v1/groups", "/api/v1/subscriptions/a/apply", "/api/v1/killswitch/config", "/api/v1/desktop/settings"} {
		if !call("PUT", p) {
			t.Fatalf("%s 应触发", p)
		}
	}
	if call("GET", "/api/v1/nodes") {
		t.Fatal("读操作不应触发")
	}
	if !call("POST", "/api/v1/killswitch/enable") {
		t.Fatal("Windows 上启用保护会改变 strict-route，应触发")
	}
	status = 400
	if call("PUT", "/api/v1/nodes/x") {
		t.Fatal("失败的写操作不应触发")
	}
}
