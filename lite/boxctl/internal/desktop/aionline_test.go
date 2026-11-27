package desktop

import (
	"slices"
	"testing"
	"time"

	"boxctl/internal/dist"
)

func TestOnlineAccounts(t *testing.T) {
	now := time.Now()
	sync := dist.AISync{Accounts: []dist.AISyncAccount{
		{ID: 1, Kind: dist.KindClaude, Email: "a@example.com"},
		{ID: 2, Kind: dist.KindClaude, Email: "b@example.com"},
		{ID: 3, Kind: dist.KindCodex, Email: "c@example.com"},
		{ID: 4, Kind: dist.KindCodex, Email: "d@example.com"},
	}}
	ids := func(list []dist.AIOnlineAccount) []int64 {
		var out []int64
		for _, a := range list {
			out = append(out, a.AccountID)
		}
		return out
	}

	// macOS：按进程。a 在运行，b 没有；Codex 默认目录里是没交给工作区的托管账号 d，在运行。Gemini 当前账号 9。
	r := &WorkspaceReport{At: now, Workspaces: []Workspace{
		{Kind: dist.KindClaude, Email: "A@example.com", Running: 2},
		{Kind: dist.KindClaude, Email: "b@example.com", Running: 0, Default: true},
		{Kind: dist.KindCodex, Email: "c@example.com", Running: 0},
		{Kind: dist.KindClaude, Email: "x@example.com", Running: 1}, // 不是托管账号
	}, Defaults: map[string]DefaultDir{
		dist.KindCodex: {Email: "d@example.com", LoggedIn: true, Running: 1},
	}}
	got := onlineAccounts(sync, true, 9, r, now)
	if !slices.Equal(ids(got), []int64{1, 4, 9}) || got[2].Kind != dist.KindGemini {
		t.Fatalf("got %+v", got)
	}

	// Windows：拿不到进程数，按设为默认的账号算。
	r.Workspaces = []Workspace{
		{Kind: dist.KindClaude, Email: "a@example.com", Running: -1},
		{Kind: dist.KindClaude, Email: "b@example.com", Running: -1, Default: true},
	}
	r.Defaults = map[string]DefaultDir{dist.KindClaude: {Email: "b@example.com", Owner: "b@example.com", LoggedIn: true, Running: -1}}
	if got := onlineAccounts(sync, true, 0, r, now); !slices.Equal(ids(got), []int64{2}) {
		t.Fatalf("windows got %+v", got)
	}

	// 外壳没在运行（扫描结果过期）或还没同步：只报 Gemini。
	r.At = now.Add(-5 * time.Minute)
	if got := onlineAccounts(sync, true, 9, r, now); !slices.Equal(ids(got), []int64{9}) {
		t.Fatalf("stale got %+v", got)
	}
	if got := onlineAccounts(sync, false, 0, nil, now); len(got) != 0 {
		t.Fatalf("unsynced got %+v", got)
	}
}
