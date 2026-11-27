package core

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"boxctl/internal/dist"
)

func conn(id, host, rule string, chains []string, up, down int64) Conn {
	return Conn{ID: id, Network: "tcp", Host: host, Destination: "1.2.3.4:443", Rule: rule, RulePayload: host,
		Chains: chains, Process: "claude", Source: "192.168.1.10", Upload: up, Download: down, Start: time.Unix(0, 0)}
}

func TestConnStatsCollects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conns.json")
	s := NewConnStats(path, "box", nil, true, nil)
	t0 := time.Date(2026, 11, 23, 8, 0, 10, 0, time.UTC)
	proxy := []string{"香港 - 01", "中转", "Claude"} // 实际出站 → … → 代理组
	snap := func(sec int, up, down int64, cs ...Conn) {
		s.Observe(ConnSnapshot{Time: t0.Add(time.Duration(sec) * time.Second), Running: true, UploadTotal: up, DownloadTotal: down, Conns: cs})
	}
	// 第一次只记现状。
	snap(0, 100, 1000, conn("a", "claude.ai", "DomainSuffix", proxy, 10, 100))
	// a 增加 20/200；新连接 b 走代理；d 是 DIRECT 不统计；内核总量多出的 5/50 是未归属。
	snap(1, 100+20+3+2+5, 1000+200+30+20+50,
		conn("a", "claude.ai", "DomainSuffix", proxy, 30, 300),
		conn("b", "", "Match", []string{"美国 - 01", "Proxy"}, 3, 30),
		conn("d", "baidu.com", "DomainSuffix", []string{"DIRECT"}, 2, 20))
	// a、d 关闭。
	snap(2, 130, 1300, conn("b", "", "Match", []string{"美国 - 01", "Proxy"}, 3, 30))
	// 下一分钟：打包。
	snap(60, 130, 1300, conn("b", "", "Match", []string{"美国 - 01", "Proxy"}, 3, 30))

	if len(s.queue) != 1 {
		t.Fatalf("queue = %d", len(s.queue))
	}
	b := s.queue[0]
	byHost := map[string]dist.ConnMinute{}
	for _, m := range b.Minutes {
		byHost[m.Host] = m
	}
	a := byHost["claude.ai"]
	if a.Upload != 20 || a.Download != 200 || a.Conns != 0 || fmt.Sprint(a.Path) != "[Claude 中转 香港 - 01]" || a.Source != "192.168.1.10" {
		t.Fatalf("a = %+v", a)
	}
	// 没有域名时用目标 IP；新连接计数。
	if m := byHost["1.2.3.4"]; m.Upload != 3 || m.Conns != 1 {
		t.Fatalf("b = %+v", m)
	}
	if _, ok := byHost["baidu.com"]; ok {
		t.Fatal("DIRECT should not be counted")
	}
	if len(b.Unattributed) != 1 || b.Unattributed[0].Upload != 5 || b.Unattributed[0].Download != 50 {
		t.Fatalf("unattributed = %+v", b.Unattributed)
	}
	// 关闭记录只有代理连接 a，流量是最后看到的累计值。
	if len(b.Records) != 1 || b.Records[0].Host != "claude.ai" || b.Records[0].Download != 300 {
		t.Fatalf("records = %+v", b.Records)
	}

	// 队列保存在磁盘上，重启后还在；序号继续递增。
	s2 := NewConnStats(path, "box", nil, true, nil)
	if len(s2.queue) != 1 || s2.lastSeq != b.Seq {
		t.Fatalf("restored = %d %d", len(s2.queue), s2.lastSeq)
	}
	// 关闭统计：清空。
	s2.SetEnabled(false)
	s3 := NewConnStats(path, "box", nil, true, nil)
	if len(s3.queue) != 0 {
		t.Fatal("disabling should clear the queue")
	}
}

func TestConnStatsDesktopAndLimits(t *testing.T) {
	s := NewConnStats("", "desktop", nil, true, nil)
	t0 := time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)
	s.Observe(ConnSnapshot{Time: t0, Running: true})
	var cs []Conn
	for i := range connStatsMinuteKeys + 50 {
		cs = append(cs, conn(fmt.Sprint(i), fmt.Sprintf("h%03d.example", i), "Match", []string{"节点", "Proxy"}, int64(1000-i), 0))
	}
	s.Observe(ConnSnapshot{Time: t0.Add(time.Second), Running: true, Conns: cs})
	// 内核停了：连接全部断开；分钟过去后由 Seal 打包。
	s.Observe(ConnSnapshot{Time: t0.Add(2 * time.Second)})
	s.now = func() time.Time { return t0.Add(time.Minute) }
	s.Seal()
	if len(s.queue) != 1 {
		t.Fatalf("queue = %d", len(s.queue))
	}
	b := s.queue[0]
	if len(b.Minutes) != connStatsMinuteKeys {
		t.Fatalf("minutes = %d", len(b.Minutes))
	}
	last := b.Minutes[len(b.Minutes)-1]
	if last.Host != connStatsOther || last.Conns != 51 || fmt.Sprint(last.Path) != "[Proxy 节点]" {
		t.Fatalf("merged = %+v", last)
	}
	// 桌面端带进程名，不带来源 IP；内核停止时还在的连接没有关闭记录（没有最终流量）。
	if b.Minutes[0].Process != "claude" || b.Minutes[0].Source != "" {
		t.Fatalf("desktop minute = %+v", b.Minutes[0])
	}
}
