package dist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"boxctl/internal/groups"
	"boxctl/internal/nodes"
	"boxctl/internal/secret"
	"boxctl/internal/store"
)

func newServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "boxctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	key, _ := secret.LoadOrCreateKey(filepath.Join(dir, "device.key"))
	box, _ := secret.New(key)
	gs := groups.New(db)
	ns := nodes.New(db, box)
	ns.Groups = gs
	return &Server{DB: db, Secrets: box, Nodes: ns, Groups: gs, Dir: filepath.Join(dir, "releases")}
}

// seed 建一个节点和一个手动选择组，返回节点 id。
func seed(t *testing.T, s *Server) string {
	t.Helper()
	ctx := context.Background()
	n, err := s.Nodes.Create(ctx, nodes.Input{Kind: nodes.KindManual, DisplayName: "US ISP",
		Mapping: map[string]any{"type": "socks5", "server": "203.0.113.9", "port": 1080, "username": "u", "password": "p"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Groups.Create(ctx, groups.Group{Name: "Claude", Type: groups.TypeSelect, Strict: true,
		Members: groups.Members{Explicit: []string{n.ID}}}); err != nil {
		t.Fatal(err)
	}
	return n.ID
}

// tlsServer 起一个自签名证书的管理服务，返回地址和证书指纹。
func tlsServer(t *testing.T, s *Server) (string, string) {
	t.Helper()
	mux := http.NewServeMux()
	s.ClientRoutes(mux, slog.New(slog.DiscardHandler))
	ts := httptest.NewTLSServer(mux)
	t.Cleanup(ts.Close)
	sum := sha256.Sum256(ts.Certificate().Raw)
	return ts.URL, hex.EncodeToString(sum[:])
}

func TestPublishAndSync(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	if _, err := s.Publish(ctx, ""); err == nil {
		t.Fatal("没有节点时不应能发布")
	}
	seed(t, s)
	if err := s.SetSelections(ctx, map[string]string{"Claude": "US ISP", "已删除的组": "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRules(ctx, []string{"DOMAIN-SUFFIX,example.com,Claude", "# 注释", ""}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStrictEgress(ctx, []string{"203.0.113.7/24"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStrictEgress(ctx, []string{"not-an-ip"}); err == nil {
		t.Fatal("无效的出口应拒绝")
	}
	if err := s.SetStrictEgress(ctx, []string{" 198.51.100.5 ", "# 注释", "198.51.100.5/32", "2001:db8::1"}); err != nil {
		t.Fatal(err)
	}
	p, err := s.Publish(ctx, "第一版")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.StrictEgress, []string{"198.51.100.5/32", "2001:db8::1/128"}) {
		t.Fatalf("严格线路出口：%v", p.StrictEgress)
	}
	if p.Version != 1 || len(p.Nodes) != 1 || p.Selections["Claude"] != "US ISP" || len(p.Selections) != 1 || len(p.Rules) != 1 {
		t.Fatalf("发布内容不对：%+v", p)
	}
	// 数据库里的配置是加密的：凭据不能以明文出现。
	var raw []byte
	_ = s.DB.QueryRow(`SELECT body FROM profiles WHERE version = 1`).Scan(&raw)
	if strings.Contains(string(raw), "203.0.113.9") {
		t.Fatal("发布的配置应加密保存")
	}

	_, token, err := s.CreateDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	url, fp := tlsServer(t, s)
	c, err := NewClient(t.TempDir(), "0.2.0", nil)
	if err != nil {
		t.Fatal(err)
	}

	// 自签名证书：不填指纹就校验失败，填错也失败。
	if _, err := c.Bind(ctx, Binding{URL: url, Token: token}); err == nil {
		t.Fatal("自签名证书不填指纹应失败")
	}
	if _, err := c.Bind(ctx, Binding{URL: url, Token: token, Fingerprint: strings.Repeat("0", 64)}); err == nil {
		t.Fatal("指纹不对应失败")
	}
	if _, err := c.Bind(ctx, Binding{URL: url, Token: "bc_wrong", Fingerprint: fp}); err == nil {
		t.Fatal("令牌不对应失败")
	}
	st, err := c.Bind(ctx, Binding{URL: url + "/", Token: token, Fingerprint: strings.ToUpper(fp[:2]) + ":" + fp[2:]})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Bound || st.Version != 1 || st.Nodes != 1 {
		t.Fatalf("绑定后状态不对：%+v", st)
	}
	got, ok := c.Profile()
	if !ok || got.Nodes[0]["password"] != "p" {
		t.Fatalf("应拿到下发的节点：%+v", got)
	}

	// 新版客户端上报设备名和系统版本，覆盖新建时的占位名称。
	name, osv := deviceInfo()
	if d := devices(t, s)[0]; d.Name != name || d.OSVersion != osv || d.AppVersion != "0.2.0" || name == "" {
		t.Fatalf("应记录客户端上报的设备信息：%+v（期望 %q %q）", d, name, osv)
	}

	// 版本没变：304，不算变化。
	if changed, err := c.Sync(ctx); err != nil || changed {
		t.Fatalf("没有新版本时不应变化：%v %v", changed, err)
	}
	devs, _ := s.Devices(ctx)
	if devs[0].AppVersion != "0.2.0" || devs[0].ProfileVersion != 1 || devs[0].LastSeen == "" {
		t.Fatalf("应记录客户端状态：%+v", devs[0])
	}

	// 新版本：拉到并报告变化。
	if _, err := s.Publish(ctx, "第二版"); err != nil {
		t.Fatal(err)
	}
	if changed, err := c.Sync(ctx); err != nil || !changed {
		t.Fatalf("有新版本时应变化：%v %v", changed, err)
	}
	if got, _ := c.Profile(); got.Version != 2 {
		t.Fatalf("应更新到第 2 版：%d", got.Version)
	}

	// 重启后从文件恢复。
	c2, err := NewClient(c.dir, "0.2.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := c2.Profile(); !ok || got.Version != 2 {
		t.Fatal("重启后应保留绑定和配置")
	}

	// 吊销后同步失败，但保留最后一次的配置继续使用。
	if err := s.RevokeDevice(ctx, devs[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, "第三版"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Sync(ctx); err == nil || !strings.Contains(c.Status().LastError, "吊销") {
		t.Fatalf("吊销后应失败：%q", c.Status().LastError)
	}
	if got, _ := c.Profile(); got.Version != 2 {
		t.Fatal("同步失败时保留上一次的配置")
	}

	if err := c.Unbind(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Profile(); ok {
		t.Fatal("解绑后回到本地配置")
	}
}

func TestMergeServerWins(t *testing.T) {
	p := Profile{
		Nodes:  []map[string]any{{"name": "A"}},
		Groups: []groups.Export{{Name: "Claude", Members: []string{"A"}}},
		Rules:  []string{"DOMAIN,a.com,Claude"},
	}
	m := Merge(p,
		[]map[string]any{{"name": "A", "local": true}, {"name": "B"}},
		[]groups.Export{{Name: "Claude", Members: []string{"B"}}, {Name: "本地组", Members: []string{"B"}}},
		[]string{"DOMAIN,b.com,DIRECT"})
	if len(m.Nodes) != 2 || m.Nodes[0]["local"] != nil {
		t.Fatalf("同名节点以下发的为准：%+v", m.Nodes)
	}
	if len(m.Groups) != 2 || m.Groups[0].Members[0] != "A" || m.Groups[1].Name != "本地组" {
		t.Fatalf("同名组以下发的为准，本地组追加：%+v", m.Groups)
	}
	if m.Rules[0] != "DOMAIN,a.com,Claude" || len(m.Rules) != 2 {
		t.Fatalf("下发的规则在前：%v", m.Rules)
	}
	if len(m.Warnings) != 1 {
		t.Fatalf("同名的本地节点应提示：%v", m.Warnings)
	}
}

func TestReleases(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	_, token, _ := s.CreateDevice(ctx)
	if _, err := s.AddRelease(ctx, "1.0", "darwin-aarch64", "", "a.tar.gz", "sig", strings.NewReader("x")); err == nil {
		t.Fatal("版本号格式不对应失败")
	}
	if _, err := s.AddRelease(ctx, "1.0.0", "linux-x86_64", "", "a.tar.gz", "sig", strings.NewReader("x")); err == nil {
		t.Fatal("未知平台应失败")
	}
	r1, err := s.AddRelease(ctx, "0.2.0", "darwin-aarch64", "修复", "../../Banbu Connect.app.tar.gz", fakeSig("0.2.0", "Banbu Connect.app.tar.gz"), strings.NewReader("pkg-0.2.0"))
	if err != nil {
		t.Fatal(err)
	}
	if r1.FileName != "Banbu_Connect.app.tar.gz" {
		t.Fatalf("文件名应清理：%q", r1.FileName)
	}
	sig2 := fakeSig("0.10.0", "b.tar.gz")
	r2, _ := s.AddRelease(ctx, "0.10.0", "darwin-aarch64", "", "b.tar.gz", sig2, strings.NewReader("pkg-0.10.0"))

	url, fp := tlsServer(t, s)
	c, _ := NewClient(t.TempDir(), "dev", nil)
	if err := s.SetSelections(ctx, nil); err != nil {
		t.Fatal(err)
	}
	seed(t, s)
	if _, err := s.Publish(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bind(ctx, Binding{URL: url, Token: token, Fingerprint: fp}); err != nil {
		t.Fatal(err)
	}

	// 没发布的更新包不下发。
	if u, err := c.CheckUpdate(ctx, "darwin", "aarch64", "0.1.0"); err != nil || u != nil {
		t.Fatalf("未发布时不应有更新：%+v %v", u, err)
	}
	_ = s.SetReleasePublished(ctx, r1.ID, true)
	_ = s.SetReleasePublished(ctx, r2.ID, true)
	u, err := c.CheckUpdate(ctx, "darwin", "aarch64", "0.1.0")
	if err != nil || u == nil || u.Version != "0.10.0" || u.Signature != sig2 {
		t.Fatalf("应给出最高版本（按数字比较）：%+v %v", u, err)
	}
	if u, _ := c.CheckUpdate(ctx, "darwin", "aarch64", "0.10.0"); u != nil {
		t.Fatal("已是最新时不应有更新")
	}
	if u, _ := c.CheckUpdate(ctx, "windows", "x86_64", "0.1.0"); u != nil {
		t.Fatal("其他平台没有更新包")
	}

	// 灰度：名单里没有这台客户端时，它只能拿到不在灰度中的最高版本，也不能下载灰度包。
	other, _, _ := s.CreateDevice(ctx)
	if err := s.SetReleaseDevices(ctx, r2.ID, []int64{other.ID, other.ID}); err != nil {
		t.Fatal(err)
	}
	if u, err := c.CheckUpdate(ctx, "darwin", "aarch64", "0.1.0"); err != nil || u == nil || u.Version != "0.2.0" {
		t.Fatalf("不在灰度名单里应拿到 0.2.0：%+v %v", u, err)
	}
	if err := c.Download(ctx, r2.ID, httptest.NewRecorder()); err == nil {
		t.Fatal("不在灰度名单里不能下载灰度包")
	}
	if list, _ := s.Releases(ctx); len(list[0].Devices) != 1 {
		t.Fatalf("灰度名单应去重：%+v", list[0].Devices)
	}
	me := devices(t, s)[1] // 倒序：[0] 是刚建的 other
	_ = s.SetReleaseDevices(ctx, r2.ID, []int64{other.ID, me.ID})
	if u, _ := c.CheckUpdate(ctx, "darwin", "aarch64", "0.1.0"); u == nil || u.Version != "0.10.0" {
		t.Fatalf("在灰度名单里应拿到 0.10.0：%+v", u)
	}
	_ = s.SetReleaseDevices(ctx, r2.ID, nil) // 清空名单即全量

	rec := httptest.NewRecorder()
	if err := c.Download(ctx, r2.ID, rec); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != "pkg-0.10.0" {
		t.Fatalf("下载内容不对：%q", rec.Body.String())
	}
	if err := s.DeleteRelease(ctx, r2.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.Download(ctx, r2.ID, httptest.NewRecorder()); err == nil {
		t.Fatal("删除后不能下载")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.10.0", "0.9.9", 1}, {"1.0.0", "1.0.0", 0}, {"1.0.0-beta", "1.0.0", -1}, {"1.0.1", "1.0.0-rc.1", 1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("%s vs %s = %d", c.a, c.b, got)
		}
	}
}

func TestProfileRoundTripKeepsPortsUsable(t *testing.T) {
	// 节点经过 JSON 后端口是 float64，生成配置时必须仍是整数。
	b, _ := json.Marshal(Profile{Nodes: []map[string]any{{"name": "A", "port": 443}}})
	var p Profile
	_ = json.Unmarshal(b, &p)
	if p.Nodes[0]["port"].(float64) != 443 {
		t.Fatal("端口应保留")
	}
	var in *InputError
	if !errors.As(error(&InputError{"x"}), &in) {
		t.Fatal("InputError 应可识别")
	}
}

// devices 按 id 倒序列出客户端（最新建的在前）。
func devices(t *testing.T, s *Server) []Device {
	t.Helper()
	list, err := s.Devices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestCreateDevicePlaceholderName(t *testing.T) {
	s := newServer(t)
	d, _, err := s.CreateDevice(context.Background())
	if err != nil || d.Name != fmt.Sprintf("待连接 #%d", d.ID) {
		t.Fatalf("新建时应有占位名称：%+v %v", d, err)
	}
}

func TestNormalizeRuleSegments(t *testing.T) {
	ok := []string{"#@segment:custom", "DOMAIN,a.com,DIRECT", "# 普通注释", "#@segment:service", "#@segment:fallback", "MATCH,漏网之鱼"}
	got, err := NormalizeRules(ok)
	if err != nil || len(got) != 5 {
		t.Fatalf("got %v %v", got, err)
	}
	if s := stripSegments(got); len(s) != 2 || s[1] != "MATCH,漏网之鱼" {
		t.Fatalf("下发时去掉分段标记：%v", s)
	}
	for name, rules := range map[string][]string{
		"MATCH 不在兜底段": {"#@segment:custom", "MATCH,DIRECT"},
		"两条 MATCH":    {"#@segment:fallback", "MATCH,DIRECT", "MATCH,X"},
		"兜底段放了别的":     {"#@segment:fallback", "DOMAIN,a,DIRECT"},
		"段顺序":         {"#@segment:proxy", "#@segment:direct"},
		"未知段":         {"#@segment:xx"},
	} {
		if _, err := NormalizeRules(rules); err == nil {
			t.Errorf("%s：应拒绝", name)
		}
	}
}

func TestRenameDevice(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	d, _, err := s.CreateDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 没改过名：上报的电脑名覆盖名称。
	if err := s.Touch(ctx, d.ID, "198.51.100.1", ClientInfo{Hostname: "MacBook-A"}, 0); err != nil {
		t.Fatal(err)
	}
	if got := deviceByID(t, s, d.ID); got.Name != "MacBook-A" || got.ReportedName != "MacBook-A" || got.NameCustom {
		t.Fatalf("自动命名：%+v", got)
	}
	// 改名后，之后上报的电脑名只更新 reported_name。
	got, err := s.RenameDevice(ctx, d.ID, "  财务-李四  ")
	if err != nil || got.Name != "财务-李四" || !got.NameCustom {
		t.Fatalf("改名：%+v %v", got, err)
	}
	_ = s.Touch(ctx, d.ID, "198.51.100.1", ClientInfo{Hostname: "MacBook-B"}, 0)
	if got := deviceByID(t, s, d.ID); got.Name != "财务-李四" || got.ReportedName != "MacBook-B" {
		t.Fatalf("改名后不应被覆盖：%+v", got)
	}
	// 清空名称：恢复自动命名，用最近上报的电脑名。
	if got, err := s.RenameDevice(ctx, d.ID, " "); err != nil || got.Name != "MacBook-B" || got.NameCustom {
		t.Fatalf("恢复自动命名：%+v %v", got, err)
	}
	// 还没上报过电脑名的客户端恢复成占位名称。
	d2, _, _ := s.CreateDevice(ctx)
	_, _ = s.RenameDevice(ctx, d2.ID, "临时")
	if got, _ := s.RenameDevice(ctx, d2.ID, ""); got.Name != fmt.Sprintf("待连接 #%d", d2.ID) {
		t.Fatalf("占位名称：%+v", got)
	}
	var in *InputError
	if _, err := s.RenameDevice(ctx, d.ID, strings.Repeat("名", maxDeviceName+1)); !errors.As(err, &in) {
		t.Fatalf("超长应报输入错误：%v", err)
	}
	if _, err := s.RenameDevice(ctx, 9999, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的客户端：%v", err)
	}
}

func deviceByID(t *testing.T, s *Server, id int64) Device {
	t.Helper()
	list, err := s.Devices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range list {
		if d.ID == id {
			return d
		}
	}
	t.Fatalf("没有客户端 %d", id)
	return Device{}
}
