package sub

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func loadSample(t *testing.T) []map[string]any {
	t.Helper()
	b, err := os.ReadFile("testdata/sample_sub.json") // 已脱敏（M13-04）
	if err != nil {
		t.Fatal(err)
	}
	raw, format, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if format != FormatClash {
		t.Fatalf("format %q", format)
	}
	return raw
}

// M2-03：解析出 125 个节点，协议分布与样本一致。
func TestParseSample(t *testing.T) {
	raw := loadSample(t)
	_, rep := Process(raw, "jsyun", Options{NameTemplate: "{seq}", KeepUnmatched: true}, nil)
	want := map[string]int{"anytls": 98, "hysteria2": 16, "vless": 6, "trojan": 5}
	if rep.Parsed != 125 {
		t.Fatalf("parsed %d", rep.Parsed)
	}
	for k, v := range want {
		if rep.Protocols[k] != v {
			t.Errorf("%s: %d, want %d", k, rep.Protocols[k], v)
		}
	}
}

func TestParseClashYAML(t *testing.T) {
	doc := `
port: 7890
proxies:
  - {name: "🇭🇰 HK 01", type: trojan, server: a.example.com, port: 443, password: p1}
  - name: "🇯🇵 JP 01"
    type: ss
    server: b.example.com
    port: "8388"
    cipher: aes-128-gcm
    password: p2
`
	raw, format, err := Parse([]byte(doc))
	if err != nil || format != FormatClash || len(raw) != 2 {
		t.Fatalf("%v %v %d", format, err, len(raw))
	}
}

const uriList = `trojan://fake-pw@t.example.com:443?sni=t.example.com#%F0%9F%87%AD%F0%9F%87%B0%20HK%20trojan
vless://00000000-0000-0000-0000-000000000001@v.example.com:443?encryption=none&security=reality&sni=www.example.com&fp=chrome&pbk=` +
	`DYc6P1WSgMqKoUcV1yBvUNhAXyL9OKYgR8rK0d0Pbkk&sid=01ab&type=tcp&flow=xtls-rprx-vision#%F0%9F%87%AF%F0%9F%87%B5%20JP%20reality
hysteria2://fake-pw@h.example.com:8443?sni=h.example.com#%F0%9F%87%BA%F0%9F%87%B8%20US%20hy2
anytls://fake-pw@a.example.com:443?sni=a.example.com#%F0%9F%87%B8%F0%9F%87%AC%20SG%20anytls
ss://YWVzLTEyOC1nY206ZmFrZS1wdw@s.example.com:8388#%F0%9F%87%B9%F0%9F%87%BC%20TW%20ss
`

func TestParseURIListPlainAndBase64(t *testing.T) {
	for name, content := range map[string]string{
		"plain":  uriList,
		"base64": base64.StdEncoding.EncodeToString([]byte(uriList)),
	} {
		raw, format, err := Parse([]byte(content))
		if err != nil || format != FormatURI {
			t.Fatalf("%s: %v %v", name, format, err)
		}
		nodes, rep := Process(raw, "x", DefaultOptions(), ValidateWithMihomo)
		if len(rep.Invalid) > 0 {
			t.Fatalf("%s: invalid %+v", name, rep.Invalid)
		}
		got := map[string]bool{}
		for _, n := range nodes {
			got[n.Protocol] = true
		}
		for _, p := range []string{"trojan", "vless", "hysteria2", "anytls", "ss"} {
			if !got[p] {
				t.Errorf("%s: missing %s (got %v)", name, p, got)
			}
		}
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, c := range []string{"", "hello world", "<html>login</html>"} {
		if _, _, err := Parse([]byte(c)); err == nil {
			t.Errorf("%q: want error", c)
		}
	}
}

// M2-05/06/08 与重复检测：样本清洗结果。
func TestProcessSample(t *testing.T) {
	raw := loadSample(t)
	nodes, rep := Process(raw, "jsyun", DefaultOptions(), ValidateWithMihomo)

	if len(rep.Invalid) != 0 {
		t.Fatalf("invalid: %+v", rep.Invalid)
	}
	if len(rep.Filtered) != 7 {
		t.Errorf("filtered %d: %v", len(rep.Filtered), rep.Filtered)
	}
	if len(rep.Unmatched) != 0 {
		t.Errorf("unmatched: %v", rep.Unmatched)
	}
	// 样本中有 4 组除名称外完全相同的节点：trojan×4（均为信息节点）、vless 2 组各 ×2、hy2 ×10
	if len(rep.Duplicates) != 11 {
		t.Errorf("duplicates %d: %v", len(rep.Duplicates), rep.Duplicates)
	}
	if rep.Kept != 107 || len(nodes) != 107 {
		t.Fatalf("kept %d", rep.Kept)
	}
	want := map[string]int{"香港": 24, "日本": 22, "新加坡": 15, "台湾": 10, "美国": 16, "韩国": 2, "印度尼西亚": 1, "印度": 1}
	for k, v := range want {
		if rep.Regions[k] != v {
			t.Errorf("region %s: %d, want %d", k, rep.Regions[k], v)
		}
	}
	if nodes[0].DisplayName != "香港 - 01 - jsyun" {
		t.Errorf("display name %q", nodes[0].DisplayName)
	}
	names := map[string]bool{}
	for _, n := range nodes {
		if names[n.DisplayName] {
			t.Errorf("duplicate display name %q", n.DisplayName)
		}
		names[n.DisplayName] = true
	}
}

func TestMatchRegion(t *testing.T) {
	cases := map[string]string{
		"🇭🇰 香港|直连家宽1":         "香港",
		"印度尼西亚 01":            "印度尼西亚", // 长关键词优先
		"印度 02":               "印度",
		"Hong Kong IPLC":      "香港",
		"US 01":               "", // 不用 2 字母代码
		"剩余流量：1200 GB":        "",
		"🇺🇸 United States 美国": "美国",
	}
	for name, want := range cases {
		if got := MatchRegion(name); got != want {
			t.Errorf("%q: %q, want %q", name, got, want)
		}
	}
}

// M2-08：只改名称不改变 ID；改凭据/端口则改变。
func TestNodeIDStable(t *testing.T) {
	base := map[string]any{"name": "A", "type": "trojan", "server": "x.example.com", "port": 443, "password": "p"}
	id := NodeID(base)
	renamed := cloneMap(base)
	renamed["name"] = "🇭🇰 完全不同的名字"
	if NodeID(renamed) != id {
		t.Fatal("rename changed id")
	}
	jsonPort := cloneMap(base)
	jsonPort["port"] = float64(443)
	if NodeID(jsonPort) != id {
		t.Fatal("port type changed id")
	}
	for k, v := range map[string]any{"password": "q", "port": 444, "server": "y.example.com"} {
		m := cloneMap(base)
		m[k] = v
		if NodeID(m) == id {
			t.Errorf("changing %s kept id", k)
		}
	}
	if !strings.HasPrefix(id, "n_") || len(id) != 18 {
		t.Fatalf("id %q", id)
	}
}

func TestSplitMergeSecrets(t *testing.T) {
	m := map[string]any{"type": "ss", "password": "p", "server": "s",
		"plugin-opts": map[string]any{"mode": "tls", "password": "pp"}}
	pub, sec := SplitSecrets(m)
	if _, ok := pub["password"]; ok {
		t.Fatal("password in public")
	}
	if _, ok := pub["plugin-opts"].(map[string]any)["password"]; ok {
		t.Fatal("nested password in public")
	}
	if m["password"] != "p" || m["plugin-opts"].(map[string]any)["password"] != "pp" {
		t.Fatal("input mutated")
	}
	back := MergeSecrets(pub, sec)
	if back["password"] != "p" || back["plugin-opts"].(map[string]any)["password"] != "pp" ||
		back["plugin-opts"].(map[string]any)["mode"] != "tls" {
		t.Fatalf("merge: %v", back)
	}
}

func TestOptionsValidate(t *testing.T) {
	if err := DefaultOptions().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []func(*Options){
		func(o *Options) { o.NameTemplate = "{region}-{sub}" },  // 无 {seq}
		func(o *Options) { o.NameTemplate = "{seq}-{country}" }, // 未知变量
		func(o *Options) { o.FilterPatterns = []string{"("} },
		func(o *Options) { o.TimeoutSec = 0 },
		func(o *Options) { o.Retries = 9 },
	}
	for i, mod := range bad {
		o := DefaultOptions()
		mod(&o)
		if o.Validate() == nil {
			t.Errorf("case %d: want error", i)
		}
	}
}

func TestParseUserInfo(t *testing.T) {
	ui := ParseUserInfo("upload=1024; download=2048; total=1.073741824E10; expire=1782950400")
	if ui == nil || ui.Upload != 1024 || ui.Download != 2048 || ui.Total != 10737418240 || ui.Expire != 1782950400 {
		t.Fatalf("%+v", ui)
	}
	if ParseUserInfo("") != nil || ParseUserInfo("foo=bar") != nil {
		t.Fatal("want nil")
	}
}

func TestFetch(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway) // 首次失败，验证重试
			return
		}
		if r.UserAgent() != "clash.meta" {
			t.Errorf("ua %q", r.UserAgent())
		}
		w.Header().Set("Subscription-Userinfo", "upload=1; download=2; total=3; expire=4")
		_, _ = w.Write([]byte("proxies: []"))
	}))
	defer srv.Close()

	res, err := Fetch(context.Background(), FetchRequest{URL: srv.URL + "/sub?token=SECRET", Timeout: 5 * time.Second, Retries: 1})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Body) != "proxies: []" || res.UserInfo == nil || res.UserInfo.Total != 3 || calls.Load() != 2 {
		t.Fatalf("%q %+v calls=%d", res.Body, res.UserInfo, calls.Load())
	}
}

func TestFetchErrorsHideURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := Fetch(context.Background(), FetchRequest{URL: srv.URL + "/sub?token=SECRET", Timeout: 5 * time.Second, Retries: 2})
	if err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err %v", err)
	}

	srv.Close()
	_, err = Fetch(context.Background(), FetchRequest{URL: srv.URL + "/sub?token=SECRET", Timeout: time.Second})
	if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("err %v", err)
	}
}

// M2-02：经代理拉取时请求发往代理地址。
func TestFetchViaProxy(t *testing.T) {
	var sawAbsolute atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAbsolute.Store(r.URL.IsAbs() && r.URL.Host == "sub.example.invalid")
		_, _ = w.Write([]byte("proxies: []"))
	}))
	defer proxy.Close()
	_, err := Fetch(context.Background(), FetchRequest{URL: "http://sub.example.invalid/x", ProxyURL: proxy.URL, Timeout: 5 * time.Second})
	if err != nil || !sawAbsolute.Load() {
		t.Fatalf("err=%v via proxy=%v", err, sawAbsolute.Load())
	}
}
