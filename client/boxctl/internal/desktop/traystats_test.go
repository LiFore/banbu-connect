package desktop

import (
	"testing"
	"time"

	"boxctl/internal/core"
)

func TestTrayStatsRate(t *testing.T) {
	var ts trayStats
	t0 := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	first := ts.next(core.ConnSnapshot{Time: t0, Running: true, UploadTotal: 1000, DownloadTotal: 5000,
		Conns: []core.Conn{{Chains: []string{"HK-01", "Proxy"}}, {Chains: []string{"DIRECT"}}}})
	if first.Up != 0 || first.Down != 0 || first.Connections != 2 || first.Proxied != 1 {
		t.Fatalf("first = %+v", first)
	}
	second := ts.next(core.ConnSnapshot{Time: t0.Add(2 * time.Second), Running: true, UploadTotal: 3000, DownloadTotal: 9000})
	if second.Up != 1000 || second.Down != 2000 {
		t.Fatalf("second = %+v", second)
	}
	// 间隔太久不算速率；内核重启（累计值回退）按 0。
	late := ts.next(core.ConnSnapshot{Time: t0.Add(time.Minute), Running: true, UploadTotal: 9000, DownloadTotal: 9999})
	reset := ts.next(core.ConnSnapshot{Time: t0.Add(61 * time.Second), Running: true, UploadTotal: 10, DownloadTotal: 10})
	if late.Up != 0 || reset.Up != 0 || reset.Down != 0 {
		t.Fatalf("late = %+v reset = %+v", late, reset)
	}
}
