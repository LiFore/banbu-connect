package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"boxctl/internal/subscriptions"
)

func TestSubscriptionAPI(t *testing.T) {
	e := newEnv(t, shortSock(t), nil)
	e.loginAndChange()

	const subURL = "https://sub.example.com/api?token=SECRET-TOKEN"
	rec := e.do("POST", "/api/v1/subscriptions", map[string]any{"name": "jsyun", "url": subURL}, true)
	if rec.Code != http.StatusCreated || strings.Contains(rec.Body.String(), "SECRET-TOKEN") {
		t.Fatalf("create %d %s", rec.Code, rec.Body)
	}
	var sb subscriptions.Subscription
	_ = json.Unmarshal(rec.Body.Bytes(), &sb)

	if rec := e.do("POST", "/api/v1/subscriptions", map[string]any{"name": "jsyun", "url": subURL}, true); errCode(t, rec) != "name_exists" {
		t.Fatalf("dup %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/api/v1/subscriptions", map[string]any{"name": "x", "url": "nope"}, true); errCode(t, rec) != "invalid" {
		t.Fatalf("invalid %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/api/v1/subscriptions", nil, false); strings.Contains(rec.Body.String(), "SECRET-TOKEN") {
		t.Fatal("list leaks url")
	}
	if rec := e.do("GET", "/api/v1/subscriptions/"+sb.ID+"?reveal=1", nil, false); !strings.Contains(rec.Body.String(), "SECRET-TOKEN") {
		t.Fatalf("reveal %s", rec.Body)
	}

	rec = e.do("POST", "/api/v1/subscriptions/"+sb.ID+"/preview", nil, true)
	var p subscriptions.Preview
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != http.StatusOK || p.Report.Kept != 107 || strings.Contains(rec.Body.String(), "fake-password") {
		t.Fatalf("preview %d kept=%d", rec.Code, p.Report.Kept)
	}
	rec = e.do("POST", "/api/v1/subscriptions/"+sb.ID+"/apply", map[string]any{"preview_id": p.ID}, true)
	var d subscriptions.Diff
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if rec.Code != http.StatusOK || len(d.Added) != 107 {
		t.Fatalf("apply %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/api/v1/subscriptions/"+sb.ID+"/apply", map[string]any{"preview_id": p.ID}, true); errCode(t, rec) != "preview_expired" {
		t.Fatalf("reapply %d %s", rec.Code, rec.Body)
	}

	if rec := e.do("DELETE", "/api/v1/subscriptions/"+sb.ID, nil, true); rec.Code != http.StatusNoContent {
		t.Fatalf("delete %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/api/v1/subscriptions/"+sb.ID, nil, false); rec.Code != http.StatusNotFound {
		t.Fatalf("get deleted %d", rec.Code)
	}
}

func TestSubscriptionDefaults(t *testing.T) {
	e := newEnv(t, shortSock(t), nil)
	e.loginAndChange()
	rec := e.do("GET", "/api/v1/subscriptions/defaults", nil, false)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "{region} - {seq:02} - {sub}") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}
