package desktop

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boxctl/internal/core"
	"boxctl/internal/killswitch"
	"boxctl/internal/trust"
)

type fakeDiagSources struct {
	ks     killswitch.Status
	net    trust.Status
	strict trust.StrictStatus
	core   core.CoreStatus
}

func newTestDiag(t *testing.T) (*diagnostics, *fakeDiagSources, *time.Time) {
	t.Helper()
	f := &fakeDiagSources{core: core.CoreStatus{Desired: true, Running: true, TUN: true}}
	f.ks = killswitch.Status{State: killswitch.StateProtecting, Health: killswitch.Health{DNSOK: true, TUNOK: true, CheckedAt: time.Now()}}
	d := newDiagnostics(t.TempDir(), nil, diagSources{
		killswitch: func() killswitch.Status { return f.ks },
		network:    func() trust.Status { return f.net },
		strict:     func() trust.StrictStatus { return f.strict },
		core:       func() core.CoreStatus { return f.core },
	}, true, nil)
	d.bound = func() bool { return true }
	clock := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return clock }
	return d, f, &clock
}

func kinds(d *diagnostics) []string {
	var out []string
	for _, r := range d.records {
		out = append(out, r.Kind+":"+r.Message)
	}
	return out
}

func TestDiagRecordsChangesNotStartup(t *testing.T) {
	d, f, clock := newTestDiag(t)
	home := trust.Network{Interface: "en0", WiFi: true, SSID: "Home Secret"}
	f.net = trust.Status{Network: home, Trusted: true, Probing: true,
		Egress: trust.Egress{OK: true, IP: "203.0.113.7", CheckedAt: *clock}}
	d.observe()
	if len(d.records) != 0 {
		t.Fatalf("第一次观察只记下状态：%v", kinds(d))
	}

	// 用户停掉代理：不记。
	f.core = core.CoreStatus{Desired: false}
	f.ks.Health.DNSOK = false
	d.observe()
	if len(d.records) != 0 {
		t.Fatalf("用户操作不应记录：%v", kinds(d))
	}
	f.core = core.CoreStatus{Desired: true, Running: true, TUN: true}
	f.ks.Health.DNSOK = true
	d.observe()

	// 内核意外退出、DNS 不可用、阻断：记，原因里不带 Wi-Fi 名称。
	f.core.Running = false
	f.ks.Health.DNSOK = false
	f.ks.State = killswitch.StateHeld
	f.ks.Verdict = killswitch.Verdict{Path: killswitch.PathHold, Reason: "Wi-Fi「Home Secret」出口不在允许列表里"}
	d.observe()
	got := strings.Join(kinds(d), "\n")
	for _, want := range []string{"killswitch:保护服务：保护中 → 已阻断", "health:Mihomo DNS 不可用", "core:代理内核意外退出"} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺少 %q：\n%s", want, got)
		}
	}
	for _, r := range d.records {
		for _, v := range r.Fields {
			if strings.Contains(v, "Home Secret") || strings.Contains(r.Message, "Home Secret") {
				t.Fatalf("不应上报 Wi-Fi 名称：%+v", r)
			}
		}
	}
	if s := d.snapshot(); strings.Contains(s.Reason, "Home Secret") || !s.Network.SSIDReadable {
		t.Fatalf("快照：%+v", s)
	}
	if d.soon == nil || len(d.soon) != 1 {
		t.Fatal("有警告时应尽快上报")
	}
}

func TestDiagEgressSampling(t *testing.T) {
	d, f, clock := newTestDiag(t)
	f.net = trust.Status{Network: trust.Network{Interface: "en0", WiFi: true, SSID: "Home"}, Trusted: true, Probing: true}
	d.observe()
	probe := func(advance time.Duration, eg trust.Egress) {
		*clock = clock.Add(advance)
		eg.CheckedAt = *clock
		f.net.Egress = eg
		d.observe()
	}
	ok := trust.Egress{OK: true, IP: "203.0.113.7", Country: "US", LatencyMS: 120}
	probe(time.Second, ok)                           // 第一次：记
	probe(30*time.Second, ok)                        // 没变：不记
	probe(30*time.Second, ok)                        // 没变：不记
	probe(10*time.Second, trust.Egress{Error: "超时"}) // 失败：记
	probe(10*time.Second, trust.Egress{Error: "超时"}) // 同样的失败：不记
	probe(10*time.Second, ok)                        // 恢复：记
	probe(diagEgressEvery, ok)                       // 15 分钟没变：记一条
	if len(d.records) != 4 {
		t.Fatalf("应记 4 条：%v", kinds(d))
	}
	last := d.records[3].Fields
	if last["probes"] != "1" || d.records[2].Fields["probes"] != "2" || d.records[2].Fields["failed"] != "1" {
		t.Fatalf("探测次数：%v / %v", d.records[2].Fields, last)
	}
}

func TestDiagDisabledOrUnboundCollectsNothing(t *testing.T) {
	d, f, _ := newTestDiag(t)
	d.observe()
	d.SetEnabled(false)
	f.ks.State = killswitch.StateHeld
	d.observe()
	if len(d.records) != 0 {
		t.Fatal("关闭后不收集")
	}
	d.SetEnabled(true)
	d.bound = func() bool { return false }
	f.ks.State = killswitch.StateProtecting
	d.observe()
	if len(d.records) != 0 {
		t.Fatal("没绑定时不收集")
	}
}

func TestDiagQueuePersists(t *testing.T) {
	d, f, _ := newTestDiag(t)
	d.observe()
	f.ks.State = killswitch.StateHeld
	d.observe()
	again := newDiagnostics(filepath.Dir(d.path), nil, d.src, true, nil)
	if len(again.records) != 1 || again.lastSeq != d.lastSeq {
		t.Fatalf("重启后应保留待上报的事件：%+v", again.records)
	}
}
