package dist

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseQuotaModels(t *testing.T) {
	// 以模型名为键；remainingFraction 为 0 时被省略。
	got := parseQuotaModels(json.RawMessage(`{
		"gemini-3-pro": {"displayName": "Gemini 3 Pro", "quotaInfo": {"remainingFraction": 0.4, "resetTime": "2026-11-23T15:00:00Z"}},
		"claude-sonnet": {"displayName": "Claude Sonnet", "quotaInfo": {"resetTime": "2026-11-23T16:00:00Z"}},
		"chat-internal": {"displayName": "无额度信息"}
	}`))
	if len(got) != 2 || got[0].Key != "claude-sonnet" || got[0].Remaining != 0 || got[1].Remaining != 0.4 || got[1].Label != "Gemini 3 Pro" {
		t.Fatalf("map form = %+v", got)
	}
	// 数组形式。
	got = parseQuotaModels(json.RawMessage(`[{"model": "m1", "quotaInfo": {"remainingFraction": 1.5}}]`))
	if len(got) != 1 || got[0].Key != "m1" || got[0].Remaining != 1 || got[0].Label != "m1" {
		t.Fatalf("list form = %+v", got)
	}
	if parseProject(json.RawMessage(`"p-1"`)) != "p-1" || parseProject(json.RawMessage(`{"id":"p-2"}`)) != "p-2" {
		t.Fatal("project forms")
	}
}

// retrieveUserQuotaSummary 的窗口归成固定名称、固定顺序；remainingFraction 省略表示用完；免费账号不显示 5 小时。
func TestParseQuotaSummary(t *testing.T) {
	raw := json.RawMessage(`{"groups": [
		{"displayName": "Gemini", "buckets": [
			{"bucketId": "gemini-weekly", "window": "weekly", "remainingFraction": 0.8, "resetTime": "2026-11-30T00:00:00Z"},
			{"bucketId": "gemini-5h", "window": "5h", "remainingFraction": 0.5}]},
		{"displayName": "Claude", "buckets": [
			{"bucketId": "3p-5h", "remainingFraction": 0.9},
			{"bucketId": "3p-weekly"}]},
		{"displayName": "Gemini 4 Argon", "buckets": [{"bucketId": "gemini-argon-wtu-weekly", "remainingFraction": 1}]},
		{"displayName": "Something New", "buckets": [{"bucketId": "nova-weekly", "remainingFraction": 0.3}]}
	]}`)
	got := parseQuotaSummary(raw, false)
	want := []struct {
		key, label string
		rem        float64
	}{
		{"claude:5h", "Claude（5 小时）", 0.9},
		{"claude:weekly", "Claude（每周）", 0},
		{"gemini:5h", "Gemini（5 小时）", 0.5},
		{"gemini:weekly", "Gemini（每周）", 0.8},
		{"argon:weekly", "Gemini 4 Argon（每周）", 1},
		{"nova-weekly", "Something New（每周）", 0.3},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i, w := range want {
		if got[i].Key != w.key || got[i].Label != w.label || got[i].Remaining != w.rem {
			t.Fatalf("[%d] = %+v, want %+v", i, got[i], w)
		}
	}
	if got[3].ResetAt != "2026-11-30T00:00:00Z" {
		t.Fatalf("reset = %q", got[3].ResetAt)
	}
	for _, b := range parseQuotaSummary(raw, true) {
		if strings.HasSuffix(b.Key, "5h") {
			t.Fatalf("free tier shows %s", b.Key)
		}
	}
	if !isFreeTier("free-tier") || isFreeTier("g1-pro-tier") || isFreeTier("g1-ultra-tier") || isFreeTier("") {
		t.Fatal("isFreeTier")
	}
	if len(parseQuotaSummary(json.RawMessage(`{}`), false)) != 0 {
		t.Fatal("empty summary")
	}
}
