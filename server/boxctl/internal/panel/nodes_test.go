package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"boxctl/internal/nodes"
)

func TestNodeAPI(t *testing.T) {
	e := newEnv(t, shortSock(t), nil)
	e.loginAndChange()

	// 导入 ISP 代理（curl 模式）
	rec := e.do("POST", "/api/v1/nodes/import", map[string]string{
		"text": `curl --proxy gw.example.com:33335 --proxy-user user-zone-isp:SECRETPW https://ip.me`}, true)
	var cands []nodes.Candidate
	_ = json.Unmarshal(rec.Body.Bytes(), &cands)
	if rec.Code != http.StatusOK || len(cands) != 1 || cands[0].Kind != nodes.KindISP || cands[0].Error != "" {
		t.Fatalf("import %d %s", rec.Code, rec.Body)
	}

	rec = e.do("POST", "/api/v1/nodes", map[string]any{
		"kind": cands[0].Kind, "display_name": "ISP 美国", "mapping": cands[0].Mapping}, true)
	if rec.Code != http.StatusCreated || strings.Contains(rec.Body.String(), "SECRETPW") {
		t.Fatalf("create %d %s", rec.Code, rec.Body)
	}
	var d nodes.Detail
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if d.Region != "美国" || len(d.Tags) != 1 || d.Tags[0] != "isp" {
		t.Fatalf("detail %+v", d)
	}

	if rec := e.do("GET", "/api/v1/nodes", nil, false); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "SECRETPW") {
		t.Fatalf("list %d", rec.Code)
	}
	if rec := e.do("GET", "/api/v1/nodes/"+d.ID+"?reveal=1", nil, false); !strings.Contains(rec.Body.String(), "SECRETPW") {
		t.Fatalf("reveal %s", rec.Body)
	}
	if rec := e.do("GET", "/api/v1/nodes/schemas", nil, false); !strings.Contains(rec.Body.String(), `"reality-opts.public-key"`) {
		t.Fatalf("schemas %s", rec.Body)
	}
	if rec := e.do("PATCH", "/api/v1/nodes/"+d.ID, map[string]any{"dialer_ref": d.ID}, true); errCode(t, rec) != "invalid" {
		t.Fatalf("self dialer %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/api/v1/nodes/batch", map[string]any{"ids": []string{d.ID}, "enabled": false}, true); !strings.Contains(rec.Body.String(), `"updated":1`) {
		t.Fatalf("batch %s", rec.Body)
	}
	// 未保存配置测试：连不上返回 test_failed 而不是 500
	rec = e.do("POST", "/api/v1/nodes/test", map[string]any{"kind": "delay",
		"mapping": map[string]any{"type": "http", "server": "127.0.0.1", "port": 1}}, true)
	if errCode(t, rec) != "test_failed" {
		t.Fatalf("test %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("DELETE", "/api/v1/nodes/"+d.ID, nil, true); rec.Code != http.StatusNoContent {
		t.Fatalf("delete %d %s", rec.Code, rec.Body)
	}
}
