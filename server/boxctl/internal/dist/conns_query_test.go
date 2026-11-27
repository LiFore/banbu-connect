package dist

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestTelemetryQueries(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	box, mac := newDevice(t, s), newDevice(t, s)
	_, _ = s.DB.Exec(`UPDATE devices SET name = '客厅路由盒' WHERE id = ?`, box.ID)
	_, _ = s.DB.Exec(`UPDATE devices SET name = 'MacBook' WHERE id = ?`, mac.ID)
	minute := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Minute)

	// 路由盒：.10 是 MacBook（闸门直连），.20 是普通设备。
	if _, err := s.RecordConns(ctx, box, ConnReport{Kind: "box", Batches: []ConnBatch{testBatch(1, minute)}}); err != nil {
		t.Fatal(err)
	}
	// MacBook 自己统计的流量（没有来源 IP），并上报局域网地址。
	if _, err := s.RecordConns(ctx, mac, ConnReport{Kind: "desktop", Batches: []ConnBatch{{
		Seq: 1, LAN: []LANAddr{{IP: "192.168.1.10", MAC: "aa:bb:cc:00:00:01"}},
		Minutes: []ConnMinute{{Minute: minute, Path: []string{"Claude", "日本 - 01"}, Host: "api.anthropic.com", Rule: "DomainKeyword",
			Payload: "anthropic", Process: "claude", Upload: 1000, Download: 9000, Conns: 3}},
	}}}); err != nil {
		t.Fatal(err)
	}
	r := ParseRange("1h", time.Now())

	o, err := s.TelemetryOverview(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if o.Total.bytes() != 110+1020+10000 || o.Unattributed != 55 || o.Clients != 2 {
		t.Fatalf("overview totals = %+v unattr %d clients %d", o.Total, o.Unattributed, o.Clients)
	}
	// 路由盒统计的 .10 归到 MacBook，和它自己统计的合在一起；.20 是路由盒下的设备。
	if o.Active != 2 || o.TopClients[0].Label != "MacBook" || o.TopClients[0].bytes() != 1100+10000 {
		t.Fatalf("clients = %+v", o.TopClients)
	}
	if o.TopClients[1].Label != "客厅路由盒 · 192.168.1.20" {
		t.Fatalf("box device = %+v", o.TopClients[1])
	}
	if len(o.Trend) < 60 {
		t.Fatalf("trend points = %d", len(o.Trend))
	}

	// 细粒度：按 MacBook 筛选，包含两路来源；带 Claude Code 会话用的 device_id。
	d, err := s.TelemetryDetail(ctx, r, TelemetryFilter{Client: fmt.Sprintf("d%d", mac.ID)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Total.bytes() != 11100 || d.DeviceID != mac.ID || len(d.Hosts) != 2 || len(d.Records) != 1 || d.Records[0].ClientLabel != "MacBook" {
		t.Fatalf("detail = total %+v dev %d hosts %+v records %+v", d.Total, d.DeviceID, d.Hosts, d.Records)
	}
	// 按链路中的一跳筛选（拓扑图点击中间节点）。
	d, _ = s.TelemetryDetail(ctx, r, TelemetryFilter{Hop: "中转"}, 0)
	if d.Total.bytes() != 1100 || len(d.Hosts) != 1 || d.Hosts[0].Key != "claude.ai" {
		t.Fatalf("hop filter = %+v %+v", d.Total, d.Hosts)
	}
	// 按规则、节点筛选。
	if d, _ = s.TelemetryDetail(ctx, r, TelemetryFilter{Rule: "Match,"}, 0); d.Total.bytes() != 30 {
		t.Fatalf("rule filter = %+v", d.Total)
	}
	if d, _ = s.TelemetryDetail(ctx, r, TelemetryFilter{Node: "日本 - 01"}, 0); d.Total.bytes() != 10000 {
		t.Fatalf("node filter = %+v", d.Total)
	}

	// 拓扑：客户端 → 中转 → 香港 - 01 → claude.ai，链式两跳都在；代理组不画。
	top, err := s.Topology(ctx, r, TelemetryFilter{}, 12)
	if err != nil {
		t.Fatal(err)
	}
	links := map[string]int64{}
	for _, l := range top.Links {
		links[l.Source+" > "+l.Target] = l.bytes()
	}
	macKey := fmt.Sprintf("c:d%d", mac.ID)
	for k, want := range map[string]int64{
		macKey + " > n1:中转":                1100,
		"n1:中转 > n2:香港 - 01":               1100,
		"n2:香港 - 01 > h:claude.ai":         1100,
		macKey + " > n1:日本 - 01":           10000,
		"n1:日本 - 01 > h:api.anthropic.com": 10000,
	} {
		if links[k] != want {
			t.Fatalf("link %s = %d, want %d (all %v)", k, links[k], want, links)
		}
	}
	for _, n := range top.Nodes {
		if n.Name == "Claude" {
			t.Fatal("group should not be drawn")
		}
	}
	// 只留前 1 个目标地址：其余合并。
	top, _ = s.Topology(ctx, r, TelemetryFilter{}, 1)
	other := false
	for _, n := range top.Nodes {
		other = other || (n.Kind == "host" && n.Name == connStatsOtherLabel)
	}
	if !other {
		t.Fatal("hosts beyond limit should merge")
	}

	// 用量：按天，MacBook 合计两路来源。
	u, err := s.Usage(ctx, time.Now().AddDate(0, 0, -1), time.Now(), "day")
	if err != nil || len(u) != 2 || u[0].Label != "MacBook" || u[0].Total.bytes() != 11100 {
		t.Fatalf("usage = %+v, %v", u, err)
	}

	// 实时：页面想看之后，客户端得知并推送。
	if s.liveWanted(box.ID) {
		t.Fatal("not watched yet")
	}
	if v := s.WatchLive(box.ID); v.At != nil {
		t.Fatal("no push yet")
	}
	res, _ := s.RecordConns(ctx, box, ConnReport{Kind: "box"})
	if !res.Live {
		t.Fatal("report result should ask for live")
	}
	s.PushLive(box.ID, []ConnRecord{{Host: "claude.ai"}})
	if v := s.WatchLive(box.ID); v.At == nil || len(v.Conns) != 1 {
		t.Fatalf("live = %+v", v)
	}
}
