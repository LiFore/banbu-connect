package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"boxctl/internal/groups"
)

func TestGroupAPI(t *testing.T) {
	e := newEnv(t, shortSock(t), nil)
	e.loginAndChange()

	rec := e.do("POST", "/api/v1/groups", map[string]any{"name": "代理模式", "type": "select",
		"members": map[string]any{"explicit": []string{"DIRECT"}}}, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %d %s", rec.Code, rec.Body)
	}
	var mode groups.View
	_ = json.Unmarshal(rec.Body.Bytes(), &mode)

	// 严格组引用含 DIRECT 的组 → 400，错误信息给出路径
	rec = e.do("POST", "/api/v1/groups", map[string]any{"name": "Claude", "type": "select", "strict": true,
		"members": map[string]any{"explicit": []string{mode.ID}}}, true)
	if errCode(t, rec) != "invalid" || !strings.Contains(rec.Body.String(), "Claude → 代理模式 → DIRECT") {
		t.Fatalf("strict %d %s", rec.Code, rec.Body)
	}

	rec = e.do("POST", "/api/v1/groups/resolve", map[string]any{"name": "预览", "type": "url-test",
		"members": map[string]any{"filter": map[string]any{"enabled": true, "regions": []string{"火星"}}}}, true)
	var v groups.View
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != http.StatusOK || !v.UsedFallback || v.Resolved[0].ID != "REJECT" || v.Options.Interval != 300 {
		t.Fatalf("resolve %d %s", rec.Code, rec.Body)
	}

	rec = e.do("POST", "/api/v1/groups/templates", map[string]any{"template": "region_manual"}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("template %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/api/v1/groups/order", map[string]any{"ids": []string{mode.ID}}, true); rec.Code != http.StatusNoContent {
		t.Fatalf("order %d", rec.Code)
	}
	if rec := e.do("DELETE", "/api/v1/groups/"+mode.ID, nil, true); rec.Code != http.StatusNoContent {
		t.Fatalf("delete %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/api/v1/groups/"+mode.ID, nil, false); rec.Code != http.StatusNotFound {
		t.Fatalf("get deleted %d", rec.Code)
	}
}
