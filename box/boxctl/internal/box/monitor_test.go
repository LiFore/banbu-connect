package box

import (
	"encoding/json"
	"testing"
	"time"
)

func snap(t *testing.T, s string) rawConns {
	t.Helper()
	var r rawConns
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMonitorAccumulates(t *testing.T) {
	m := newMonitor(nil)
	t0 := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	m.ingest(t0, true, snap(t, `{"uploadTotal":100,"downloadTotal":1000,"connections":[
		{"id":"a","metadata":{"network":"tcp","sourceIP":"192.168.1.10","destinationIP":"1.1.1.1","destinationPort":"443","host":"claude.ai"},
		 "upload":10,"download":100,"start":"2026-11-17T07:59:00Z","chains":["香港 - 01","Claude"],"rule":"DomainSuffix","rulePayload":"claude.ai"},
		{"id":"b","metadata":{"network":"udp","sourceIP":"192.168.1.11","destinationIP":"8.8.8.8","destinationPort":"53"},
		 "upload":5,"download":5,"start":"2026-11-17T07:59:30Z","chains":["DIRECT"],"rule":"Match","rulePayload":""}]}`))
	// 一秒后：a 继续传输，b 已关闭，新来一条 c 命中同一规则。
	m.ingest(t0.Add(time.Second), true, snap(t, `{"uploadTotal":300,"downloadTotal":3000,"connections":[
		{"id":"a","metadata":{"sourceIP":"192.168.1.10"},"upload":30,"download":400,"start":"2026-11-17T07:59:00Z","chains":["香港 - 01","Claude"],"rule":"DomainSuffix","rulePayload":"claude.ai"},
		{"id":"c","metadata":{"sourceIP":"192.168.1.10"},"upload":1,"download":2,"start":"2026-11-17T08:00:00Z","chains":["美国 - 01","Claude"],"rule":"DomainSuffix","rulePayload":"claude.ai"}]}`))

	live, _ := m.Live()
	if live.Up != 200 || live.Down != 2000 {
		t.Fatalf("速率 = %d/%d", live.Up, live.Down)
	}
	if live.Total != 2 || live.Connections[0].ID != "c" {
		t.Fatalf("连接按开始时间倒序：%+v", live.Connections)
	}
	dev := m.Devices()
	if d := dev["192.168.1.10"]; d.Upload != 31 || d.Download != 402 || d.Connections != 2 {
		t.Fatalf("设备 10：%+v", d)
	}
	if d := dev["192.168.1.11"]; d.Upload != 5 || d.Download != 5 || d.Connections != 0 {
		t.Fatalf("已关闭连接的用量应保留：%+v", d)
	}
	hits := m.RuleHits()
	if len(hits) != 2 || hits[0].Payload != "claude.ai" || hits[0].Hits != 2 || hits[0].Target != "Claude" {
		t.Fatalf("规则命中：%+v", hits)
	}

	// 内核停止：速率归零，累计保留。
	m.ingest(t0.Add(2*time.Second), false, rawConns{})
	live, _ = m.Live()
	if live.Running || live.Up != 0 || len(live.Devices) != 2 {
		t.Fatalf("内核停止后：%+v", live)
	}
}
