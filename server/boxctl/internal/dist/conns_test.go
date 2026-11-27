package dist

import (
	"context"
	"testing"
	"time"
)

func testBatch(seq int64, minute time.Time) ConnBatch {
	return ConnBatch{
		Seq: seq,
		Minutes: []ConnMinute{
			{Minute: minute, Path: []string{"Claude", "中转", "香港 - 01"}, Host: "claude.ai", Rule: "DomainSuffix", Payload: "claude.ai",
				Source: "192.168.1.10", Upload: 100, Download: 1000, Conns: 2},
			{Minute: minute, Path: []string{"Proxy", "美国 - 01"}, Host: "github.com", Rule: "Match", Source: "192.168.1.20", Upload: 10, Download: 20, Conns: 1},
		},
		Unattributed: []ConnUnattributed{{Minute: minute, Upload: 5, Download: 50}},
		Records: []ConnRecord{{Start: minute.Add(-time.Minute), End: minute.Add(30 * time.Second), Network: "tcp", Host: "claude.ai",
			Destination: "1.2.3.4:443", Rule: "DomainSuffix", Payload: "claude.ai", Path: []string{"Claude", "中转", "香港 - 01"},
			Source: "192.168.1.10", Upload: 50, Download: 500}},
		Sources: []ConnSource{{IP: "192.168.1.10", MAC: "AA:BB:CC:00:00:01", Name: "书房 Mac"}},
	}
}

func TestRecordConns(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	box := newDevice(t, s)
	minute := time.Now().UTC().Truncate(time.Minute).Add(-time.Minute)

	res, err := s.RecordConns(ctx, box, ConnReport{Kind: "box", Batches: []ConnBatch{testBatch(7, minute)}})
	if err != nil || res.Acked != 7 {
		t.Fatalf("record = %+v, %v", res, err)
	}
	// 重复上报（客户端没收到响应时重试）：不重复入库。
	if res, _ = s.RecordConns(ctx, box, ConnReport{Kind: "box", Batches: []ConnBatch{testBatch(7, minute)}}); res.Acked != 7 {
		t.Fatalf("dup acked = %d", res.Acked)
	}
	var n, up, down int64
	_ = s.DB.QueryRow(`SELECT COUNT(*), SUM(upload), SUM(download) FROM conn_minutes WHERE device_id = ?`, box.ID).Scan(&n, &up, &down)
	if n != 2 || up != 110 || down != 1020 {
		t.Fatalf("minutes = %d %d %d", n, up, down)
	}
	var exit, grp, mac, name string
	_ = s.DB.QueryRow(`SELECT exit_node, grp, source_mac, source_name FROM conn_minutes WHERE host = 'claude.ai'`).Scan(&exit, &grp, &mac, &name)
	if exit != "香港 - 01" || grp != "Claude" || mac != "aa:bb:cc:00:00:01" || name != "书房 Mac" {
		t.Fatalf("minute row = %s %s %s %s", exit, grp, mac, name)
	}
	// 小时用量按来源设备（有 MAC 用 MAC，没有用 IP）。
	_ = s.DB.QueryRow(`SELECT COUNT(*), SUM(download) FROM conn_hours WHERE device_id = ?`, box.ID).Scan(&n, &down)
	if n != 2 || down != 1020 {
		t.Fatalf("hours = %d %d", n, down)
	}
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM conn_hours WHERE source_key IN ('aa:bb:cc:00:00:01', '192.168.1.20')`).Scan(&n)
	if n != 2 {
		t.Fatalf("hour keys = %d", n)
	}
	// 按代理组 / 节点的小时流量：路径上每一跳都算（中转节点也算），重复上报不重复计。
	for name, want := range map[string]int64{"Claude": 1000, "中转": 1000, "香港 - 01": 1000, "美国 - 01": 20, "Proxy": 20} {
		var got int64
		_ = s.DB.QueryRow(`SELECT download FROM traffic_hours WHERE name = ?`, name).Scan(&got)
		if got != want {
			t.Fatalf("traffic_hours[%s] = %d, want %d", name, got, want)
		}
	}
	// 同一小时的下一批累加。
	_, _ = s.RecordConns(ctx, box, ConnReport{Kind: "box", Batches: []ConnBatch{testBatch(8, minute)}})
	_ = s.DB.QueryRow(`SELECT SUM(download) FROM conn_hours WHERE device_id = ?`, box.ID).Scan(&down)
	if down != 2040 {
		t.Fatalf("hours after 2nd = %d", down)
	}
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM conn_records`).Scan(&n)
	if n != 2 {
		t.Fatalf("records = %d", n)
	}
	// 桌面端上报自己的局域网地址：替换旧的。
	mac1 := newDevice(t, s)
	_, _ = s.RecordConns(ctx, mac1, ConnReport{Kind: "desktop", Batches: []ConnBatch{{Seq: 1, LAN: []LANAddr{{IP: "192.168.1.10", MAC: "AA:BB:CC:00:00:01"}}}}})
	_, _ = s.RecordConns(ctx, mac1, ConnReport{Kind: "desktop", Batches: []ConnBatch{{Seq: 2, LAN: []LANAddr{{IP: "192.168.1.11", MAC: "aa:bb:cc:00:00:01"}}}}})
	var ip string
	_ = s.DB.QueryRow(`SELECT COUNT(*), MAX(ip) FROM device_lan WHERE device_id = ?`, mac1.ID).Scan(&n, &ip)
	if n != 1 || ip != "192.168.1.11" {
		t.Fatalf("lan = %d %s", n, ip)
	}
	if _, err := s.RecordConns(ctx, box, ConnReport{Batches: make([]ConnBatch, maxConnBatches+1)}); err == nil {
		t.Fatal("too many batches should fail")
	}
}
