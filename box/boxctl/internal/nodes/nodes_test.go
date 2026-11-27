package nodes

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"boxctl/internal/groups"
	"boxctl/internal/refs"
	"boxctl/internal/secret"
	"boxctl/internal/store"
)

// connectProxy 是测试用的 HTTP CONNECT 代理，记录收到的 CONNECT 目标。
type connectProxy struct {
	*httptest.Server
	mu      sync.Mutex
	targets []string
}

func newConnectProxy(t *testing.T) *connectProxy {
	p := &connectProxy{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		p.mu.Lock()
		p.targets = append(p.targets, r.Host)
		p.mu.Unlock()
		dst, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			dst.Close()
			return
		}
		go func() { _, _ = io.Copy(dst, buf); dst.Close() }()
		_, _ = io.Copy(conn, dst)
		conn.Close()
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *connectProxy) mapping() map[string]any {
	u, _ := url.Parse(p.URL)
	port, _ := strconv.Atoi(u.Port())
	return map[string]any{"type": "http", "server": u.Hostname(), "port": port}
}

func (p *connectProxy) addr() string { u, _ := url.Parse(p.URL); return u.Host }

func (p *connectProxy) saw() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...)
}

func newService(t *testing.T) (*Service, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "boxctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	key, _ := secret.LoadOrCreateKey(filepath.Join(dir, "device.key"))
	box, _ := secret.New(key)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json" {
			_, _ = w.Write([]byte(`{"ip":"203.0.113.7","country":"US","region":"California","city":"LA","org":"AS7922 Comcast Cable"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)
	s := New(db, box)
	s.DelayURL = target.URL + "/generate_204"
	s.ExitURL = target.URL + "/json"
	return s, target
}

func TestProbeChain(t *testing.T) {
	s, target := newService(t)
	ctx := context.Background()
	airport, isp := newConnectProxy(t), newConnectProxy(t)

	a, err := s.Create(ctx, Input{Kind: KindManual, DisplayName: "🇭🇰 机场 HK", Mapping: airport.mapping()})
	if err != nil {
		t.Fatal(err)
	}
	ispMap := isp.mapping()
	ispMap["username"], ispMap["password"] = "u", "secret-pw"
	b, err := s.Create(ctx, Input{Kind: KindISP, DisplayName: "ISP US", Mapping: ispMap, DialerRef: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	if b.DialerName != "🇭🇰 机场 HK" || b.Region != "其他" || a.Region != "香港" {
		t.Fatalf("dialer=%q region=%q/%q", b.DialerName, b.Region, a.Region)
	}

	res, err := s.Test(ctx, TestRequest{Kind: "delay", ID: b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.DelayMS <= 0 || len(res.Chain) != 2 || res.Chain[1] != "🇭🇰 机场 HK" {
		t.Fatalf("delay %+v", res)
	}
	// 经机场代理连到 ISP 代理，再由 ISP 代理连目标
	targetAddr := target.Listener.Addr().String()
	if got := airport.saw(); len(got) == 0 || got[0] != isp.addr() {
		t.Fatalf("airport saw %v, want %s", got, isp.addr())
	}
	if got := isp.saw(); len(got) == 0 || got[0] != targetAddr {
		t.Fatalf("isp saw %v, want %s", got, targetAddr)
	}

	res, err = s.Test(ctx, TestRequest{Kind: "exit", ID: b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Exit == nil || res.Exit.IP != "203.0.113.7" || res.Exit.ASN != "AS7922" || res.Exit.Org != "Comcast Cable" {
		t.Fatalf("exit %+v", res.Exit)
	}

	// 未保存的表单配置也能测（掩码密码用已保存值还原）
	form := ispMap
	form["password"] = secretMaskStr
	none := ""
	if _, err := s.Test(ctx, TestRequest{Kind: "delay", ID: b.ID, Mapping: form, DialerRef: &none}); err != nil {
		t.Fatalf("form test: %v", err)
	}
}

func TestProbeFailure(t *testing.T) {
	s, _ := newService(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().(*net.TCPAddr)
	ln.Close()
	_, err := s.Test(context.Background(), TestRequest{Kind: "delay",
		Mapping: map[string]any{"type": "http", "server": "127.0.0.1", "port": dead.Port}})
	if err == nil {
		t.Fatal("want error for dead proxy")
	}
}

func TestCreateValidateMaskUpdate(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	m := map[string]any{"type": "trojan", "server": "t.example.com", "port": 443, "password": "real-secret", "sni": "t.example.com"}

	d, err := s.Create(ctx, Input{Kind: KindManual, DisplayName: "🇯🇵 自建 JP", Mapping: m, Tags: []string{" a ", "a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Region != "日本" || d.Mapping["password"] != secretMaskStr || len(d.Tags) != 2 || !d.Enabled {
		t.Fatalf("created %+v", d)
	}
	full, _ := s.Get(ctx, d.ID, true)
	if full.Mapping["password"] != "real-secret" {
		t.Fatal("reveal failed")
	}
	var ct []byte
	_ = s.db.QueryRow(`SELECT secret FROM proxy_nodes WHERE id = ?`, d.ID).Scan(&ct)
	var specHit int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM proxy_nodes WHERE spec LIKE '%real-secret%'`).Scan(&specHit)
	if bytes.Contains(ct, []byte("real-secret")) || specHit != 0 {
		t.Fatal("secret stored in plaintext")
	}

	// 编辑时密码保持掩码 → 保留原值
	upd := d.Mapping
	upd["sni"] = "new.example.com"
	if _, err := s.Update(ctx, d.ID, Input{Kind: KindManual, DisplayName: "🇯🇵 自建 JP", Mapping: upd}); err != nil {
		t.Fatal(err)
	}
	full, _ = s.Get(ctx, d.ID, true)
	if full.Mapping["password"] != "real-secret" || full.Mapping["sni"] != "new.example.com" {
		t.Fatalf("after update %+v", full.Mapping)
	}

	var ve *ValidationError
	bad := []Input{
		{Kind: KindManual, DisplayName: "🇯🇵 自建 JP", Mapping: m},                                                     // 重名
		{Kind: KindManual, DisplayName: "DIRECT", Mapping: m},                                                       // 保留名
		{Kind: "subscription", DisplayName: "x", Mapping: m},                                                        // 类型
		{Kind: KindManual, DisplayName: "x", Mapping: map[string]any{"type": "trojan", "server": "a", "port": 443}}, // 缺密码
		{Kind: KindManual, DisplayName: "y", Mapping: map[string]any{"type": "trojan", "server": "a", "port": 0, "password": "p"}},
	}
	for i, in := range bad {
		if _, err := s.Create(ctx, in); !errors.As(err, &ve) {
			t.Errorf("case %d: %v", i, err)
		}
	}
}

func TestISPTagAndDialerCycle(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	mk := func(name string) Detail {
		d, err := s.Create(ctx, Input{Kind: KindISP, DisplayName: name,
			Mapping: map[string]any{"type": "http", "server": name + ".example.com", "port": 8080}})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	a, b, c := mk("a"), mk("b"), mk("c")
	if len(a.Tags) != 1 || a.Tags[0] != TagISP {
		t.Fatalf("tags %v", a.Tags)
	}
	ref := func(id string) *string { return &id }
	if _, err := s.Patch(ctx, b.ID, Attrs{DialerRef: ref(a.ID)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Patch(ctx, c.ID, Attrs{DialerRef: ref(b.ID)}); err != nil {
		t.Fatal(err)
	}
	var ve *ValidationError
	if _, err := s.Patch(ctx, a.ID, Attrs{DialerRef: ref(c.ID)}); !errors.As(err, &ve) {
		t.Fatalf("cycle a→c→b→a: %v", err)
	}
	if _, err := s.Patch(ctx, a.ID, Attrs{DialerRef: ref(a.ID)}); !errors.As(err, &ve) {
		t.Fatalf("self: %v", err)
	}
	if _, err := s.Patch(ctx, a.ID, Attrs{DialerRef: ref("n_missing")}); !errors.As(err, &ve) {
		t.Fatalf("missing: %v", err)
	}
	// 去掉 isp 标签无效：ISP 节点总带 isp
	empty := []string{}
	n, _ := s.Patch(ctx, a.ID, Attrs{Tags: &empty})
	if len(n.Tags) != 1 {
		t.Fatalf("isp tag removed: %v", n.Tags)
	}

	// 删除被引用节点需确认
	var re *refs.Error
	if err := s.Delete(ctx, a.ID, false); !errors.As(err, &re) || re.Impacts[0].RefName != "b" {
		t.Fatalf("delete referenced: %v", err)
	}
	if err := s.Delete(ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}

	cnt, err := s.SetEnabled(ctx, []string{b.ID, c.ID}, false)
	if err != nil || cnt != 2 {
		t.Fatalf("batch %d %v", cnt, err)
	}
	list, _ := s.List(ctx)
	for _, n := range list {
		if n.Enabled {
			t.Fatalf("still enabled: %s", n.DisplayName)
		}
	}
}

// 前置为代理组：测试时取组的首个节点成员（M3-06 / M4）。
func TestProbeGroupDialer(t *testing.T) {
	s, target := newService(t)
	ctx := context.Background()
	gs := groups.New(s.db)
	s.Groups = gs
	airport, isp := newConnectProxy(t), newConnectProxy(t)

	a, err := s.Create(ctx, Input{Kind: KindManual, DisplayName: "🇭🇰 机场 HK", Mapping: airport.mapping()})
	if err != nil {
		t.Fatal(err)
	}
	g, err := gs.Create(ctx, groups.Group{Name: "链式拨号代理组", Type: groups.TypeSelect,
		Members: groups.Members{Filter: groups.Filter{Enabled: true, ExcludeTags: []string{TagISP}}}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(ctx, Input{Kind: KindISP, DisplayName: "ISP US", Mapping: isp.mapping(), DialerRef: g.ID})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Test(ctx, TestRequest{Kind: "delay", ID: b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Chain) != 2 || !strings.Contains(res.Chain[1], "链式拨号代理组") || !strings.Contains(res.Chain[1], a.DisplayName) {
		t.Fatalf("chain %v", res.Chain)
	}
	if got := airport.saw(); len(got) == 0 || got[0] != isp.addr() {
		t.Fatalf("airport saw %v", got)
	}
	if got := isp.saw(); len(got) == 0 || got[0] != target.Listener.Addr().String() {
		t.Fatalf("isp saw %v", got)
	}
}

func TestDeleteMany(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	mk := func(name string) Detail {
		d, err := s.Create(ctx, Input{Kind: KindISP, DisplayName: name,
			Mapping: map[string]any{"type": "http", "server": name + ".example.com", "port": 8080}})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	a, b, c := mk("a"), mk("b"), mk("c")
	ref := func(id string) *string { return &id }
	if _, err := s.Patch(ctx, b.ID, Attrs{DialerRef: ref(a.ID)}); err != nil {
		t.Fatal(err)
	}
	// a 被 b 引用作前置：只删 a 要确认，且确认前什么都不删。
	var re *refs.Error
	if _, _, err := s.DeleteMany(ctx, []string{a.ID, c.ID}, false); !errors.As(err, &re) {
		t.Fatalf("want refs error: %v", err)
	}
	if list, _ := s.List(ctx); len(list) != 3 {
		t.Fatalf("nothing should be deleted before confirm, got %d", len(list))
	}
	// 选中的节点之间互相引用不算。
	n, skipped, err := s.DeleteMany(ctx, []string{a.ID, b.ID, "n_missing"}, false)
	if err != nil || n != 2 || skipped != 0 {
		t.Fatalf("delete a,b: %d %d %v", n, skipped, err)
	}
	if list, _ := s.List(ctx); len(list) != 1 || list[0].ID != c.ID {
		t.Fatalf("left: %+v", list)
	}
}
