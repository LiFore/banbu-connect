package dist

import (
	"context"
	"testing"
	"time"
)

func TestDeviceAccess(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	d := newDevice(t, s)
	t0 := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	report := func(m int, st AccessState) {
		t.Helper()
		if err := s.RecordAccess(ctx, d.ID, st, at(m)); err != nil {
			t.Fatal(err)
		}
	}
	box := AccessState{Mode: AccessBox, BoxID: "home-box", BoxAddr: "192.168.10.100", CoreRunning: true, TUN: true, Gate: true}
	local := AccessState{Mode: AccessLocal, CoreRunning: true, TUN: true}

	// 0–10 分钟经路由盒，10–20 本机代理，之后离线（最后一次上报 20），30 分钟重新上线仍是本机代理。
	for m := 0; m <= 10; m++ {
		report(m, box)
	}
	for m := 11; m <= 20; m++ {
		report(m, local)
	}
	report(30, local)

	if err := s.RecordAccess(ctx, d.ID, AccessState{Mode: "bogus"}, at(31)); err == nil {
		t.Fatal("无效的接入方式应拒绝")
	}

	hist, err := s.AccessHistory(ctx, d.ID, at(-60), at(31))
	if err != nil {
		t.Fatal(err)
	}
	want := []AccessSession{
		{Mode: AccessLocal, StartAt: onlineTime(at(30)), EndAt: "", LastAt: onlineTime(at(30))},
		{Mode: AccessLocal, StartAt: onlineTime(at(11)), EndAt: onlineTime(at(20)), LastAt: onlineTime(at(20))},
		{Mode: AccessBox, BoxID: "home-box", StartAt: onlineTime(at(0)), EndAt: onlineTime(at(11)), LastAt: onlineTime(at(11))},
	}
	if len(hist) != len(want) {
		t.Fatalf("分段：%+v", hist)
	}
	for i := range want {
		if hist[i] != want[i] {
			t.Fatalf("第 %d 段 = %+v，想要 %+v", i, hist[i], want[i])
		}
	}

	list, err := s.DeviceAccesses(ctx, at(31))
	if err != nil || len(list) != 1 {
		t.Fatalf("%+v %v", list, err)
	}
	if c := list[0]; c.State.Mode != AccessLocal || c.Since != onlineTime(at(30)) || !c.Online {
		t.Fatalf("快照：%+v", c)
	}
	if list, _ := s.DeviceAccesses(ctx, at(40)); list[0].Online {
		t.Fatal("超过 3 分钟没上报应算离线")
	}
	// 离线后未结束的分段按最后一次上报结束显示
	if hist, _ := s.AccessHistory(ctx, d.ID, at(-60), at(40)); hist[0].EndAt != onlineTime(at(30)) {
		t.Fatalf("%+v", hist[0])
	}
}
