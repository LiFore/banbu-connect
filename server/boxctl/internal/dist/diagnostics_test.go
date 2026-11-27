package dist

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDiagnosticsUploadAndQuery(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	d, token, _ := s.CreateDevice(ctx)
	seed(t, s)
	if _, err := s.Publish(ctx, ""); err != nil {
		t.Fatal(err)
	}
	url, fp := tlsServer(t, s)
	c, _ := NewClient(t.TempDir(), "0.1.0", nil)

	// 没绑定时不上报。
	if sent, err := c.Report(ctx, DiagReport{}); sent || err != nil {
		t.Fatalf("未绑定不应上报：%v %v", sent, err)
	}
	if _, err := c.Bind(ctx, Binding{URL: url, Token: token, Fingerprint: fp}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rec := func(seq int64, kind, msg string) DiagRecord {
		return DiagRecord{Seq: seq, Time: now.Add(time.Duration(seq) * time.Second), Kind: kind, Level: "info", Message: msg,
			Fields: map[string]string{"ip": "203.0.113.7"}}
	}
	report := DiagReport{
		Snapshot: DiagSnapshot{KillSwitch: "direct", Network: DiagNet{Interface: "en0", WiFi: true, Trusted: true}},
		Records:  []DiagRecord{rec(1, "egress", "出口 203.0.113.7"), rec(2, "network", "网络变化")},
	}
	if sent, err := c.Report(ctx, report); !sent || err != nil {
		t.Fatalf("上报失败：%v %v", sent, err)
	}
	// 重传（上次的响应丢了）按 seq 去重。
	report.Records = append(report.Records, rec(3, "egress", "出口探测失败"))
	if _, err := c.Report(ctx, report); err != nil {
		t.Fatal(err)
	}

	got, err := s.Diagnostics(ctx, d.ID, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 3 || got.Records[0].Seq != 3 || got.Snapshot == nil || got.Snapshot.KillSwitch != "direct" {
		t.Fatalf("查询结果：%+v", got)
	}
	if got.Records[0].Fields["ip"] != "203.0.113.7" {
		t.Fatalf("字段：%+v", got.Records[0].Fields)
	}
	only, _ := s.Diagnostics(ctx, d.ID, "network", 0, 0)
	if len(only.Records) != 1 {
		t.Fatalf("按类型筛选：%+v", only.Records)
	}
	devs, _ := s.Devices(ctx)
	if devs[0].DiagAt == "" {
		t.Fatal("应记录最近上报时间")
	}

	// 格式不对的拒绝。
	bad := DiagReport{Records: []DiagRecord{{Seq: 9, Time: now, Kind: "Bad Kind", Level: "info"}}}
	if _, err := c.Report(ctx, bad); err == nil || !strings.Contains(err.Error(), "无效") {
		t.Fatalf("应拒绝：%v", err)
	}

	// 删除客户端时一并删除诊断数据。
	if err := s.DeleteDevice(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM device_diagnostics`).Scan(&n)
	if n != 0 {
		t.Fatalf("应随客户端删除，剩 %d 条", n)
	}
}

func TestDiagnosticsRetention(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	d, _, _ := s.CreateDevice(ctx)
	old := DiagRecord{Seq: 1, Time: time.Now().AddDate(0, 0, -diagKeepDays-1), Kind: "egress", Level: "info", Message: "旧"}
	fresh := DiagRecord{Seq: 2, Time: time.Now(), Kind: "egress", Level: "info", Message: strings.Repeat("长", maxDiagText+10)}
	if err := s.SaveDiagnostics(ctx, d.ID, DiagReport{Records: []DiagRecord{old, fresh}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Diagnostics(ctx, d.ID, "", 0, 0)
	if len(got.Records) != 1 || got.Records[0].Seq != 2 {
		t.Fatalf("过期的应清理：%+v", got.Records)
	}
	if n := len([]rune(got.Records[0].Message)); n != maxDiagText+1 {
		t.Fatalf("过长的消息应截断，实际 %d 字", n)
	}
}
