package core

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"boxctl/internal/gen"
)

// fakeCore 模拟内核控制接口：规则列表、规则开关、连接列表和断开连接。
type fakeCore struct {
	mu       sync.Mutex
	patches  []map[string]bool
	deleted  []string
	requests int
}

func (f *fakeCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/rules":
		_, _ = io.WriteString(w, `{"rules":[
			{"index":0,"type":"DomainSuffix","payload":"claude.ai","proxy":"`+gen.HoldProxyName+`"},
			{"index":1,"type":"DomainSuffix","payload":"claude.ai","proxy":"Claude"},
			{"index":2,"type":"SubRules","payload":"(SRC-IP-CIDR,192.168.1.2/32)","proxy":"`+gen.HoldProxyName+`·dev1"},
			{"index":3,"type":"SubRules","payload":"(SRC-IP-CIDR,192.168.1.2/32)","proxy":"dev1"},
			{"index":4,"type":"Match","payload":"","proxy":"REJECT"}]}`)
	case r.Method == http.MethodPatch && r.URL.Path == "/rules/disable":
		var m map[string]bool
		_ = json.NewDecoder(r.Body).Decode(&m)
		f.patches = append(f.patches, m)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/connections":
		_, _ = io.WriteString(w, `{"connections":[
			{"id":"a","chains":["香港 - 01","Claude"]},
			{"id":"b","chains":["DIRECT"]}]}`)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/connections/"):
		f.deleted = append(f.deleted, strings.TrimPrefix(r.URL.Path, "/connections/"))
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func TestSetHold(t *testing.T) {
	fc := &fakeCore{}
	srv := httptest.NewServer(fc)
	defer srv.Close()

	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Skip("没有 sleep：", err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	s := &Supervisor{log: slog.New(slog.DiscardHandler), secret: "s", controller: strings.TrimPrefix(srv.URL, "http://"),
		child: child, applied: []byte("cfg"), probes: map[string]int{"Claude": 19300}}
	ctx := context.Background()

	// 放行：只关掉副本（0、2），原规则和用户自己的 REJECT 不动。
	if err := s.SetHold(ctx, false); err != nil {
		t.Fatal(err)
	}
	if len(fc.patches) != 1 || len(fc.patches[0]) != 2 || !fc.patches[0]["0"] || !fc.patches[0]["2"] {
		t.Fatalf("放行时应关掉副本：%v", fc.patches)
	}
	// 同一份配置、同一状态不重复下发。
	before := fc.requests
	if err := s.SetHold(ctx, false); err != nil || fc.requests != before {
		t.Fatalf("重复下发：%v，请求 %d → %d", err, before, fc.requests)
	}
	// 阻断：打开副本，并断开经严格组的连接。
	if err := s.SetHold(ctx, true); err != nil {
		t.Fatal(err)
	}
	if last := fc.patches[len(fc.patches)-1]; len(last) != 2 || last["0"] || last["2"] {
		t.Fatalf("阻断时应打开副本：%v", last)
	}
	if strings.Join(fc.deleted, ",") != "a" {
		t.Fatalf("应只断开经严格组的连接：%v", fc.deleted)
	}
	// 换了配置（内核重新加载，副本默认生效）后要重新下发。
	s.mu.Lock()
	s.applied = []byte("cfg2")
	s.mu.Unlock()
	n := len(fc.patches)
	if err := s.SetHold(ctx, true); err != nil || len(fc.patches) != n+1 {
		t.Fatalf("换配置后应重新下发：%v，%d", err, len(fc.patches))
	}
	// 内核没在运行：什么也不做。
	s.mu.Lock()
	s.child = nil
	s.mu.Unlock()
	before = fc.requests
	if err := s.SetHold(ctx, false); err != nil || fc.requests != before {
		t.Fatalf("内核没在运行时不应请求：%v", err)
	}
}
