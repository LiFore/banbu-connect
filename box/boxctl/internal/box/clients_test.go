package box

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"boxctl/internal/boxfind"
)

// 设备页里的客户端：复查过、自报过的挂到同 IP 的设备上；租约里没有的（跨网段）单列；
// 面板刚重启没有记录、但正经代理入口上网的也算；太久没来的不再显示，超过在线时限的显示离线。
func TestDeviceClients(t *testing.T) {
	b, _, _ := setup(t)
	ctx := context.Background()
	leases := filepath.Join(b.opts.DataDir, "leases")
	_ = os.WriteFile(leases, []byte("1763400000 aa:bb:cc:00:00:01 192.168.1.10 macbook *\n1763400000 aa:bb:cc:00:00:02 192.168.1.11 nas *\n"), 0o600)
	b.discovery = discovery{leasesPath: leases, neighbors: func(context.Context) []neighbor { return nil }}

	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	b.clients = newClientRegistry()
	b.clients.now = func() time.Time { return now }
	b.clients.Hello(netip.MustParseAddr("192.168.1.10"), boxfind.Hello{Name: "小王的 MacBook", OS: "macOS 15.2", Version: "0.2.22"})
	b.clients.Seen(netip.MustParseAddr("10.8.0.5")) // 旧版客户端，跨网段
	b.clients.Seen(netip.MustParseAddr("192.168.1.30"))
	b.clients.now = func() time.Time { return now.Add(8 * 24 * time.Hour) }
	b.clients.Seen(netip.MustParseAddr("192.168.1.10")) // 重新来过：在线，自报的名字保留
	b.clients.now = func() time.Time { return now.Add(8*24*time.Hour + 2*time.Minute) }

	// 192.168.1.11 正经代理入口上网，但没有记录（面板刚重启）。
	b.monitor = newMonitor(nil)
	b.monitor.ingest(now, true, snap(t, `{"connections":[
		{"id":"a","metadata":{"sourceIP":"192.168.1.11","inboundName":"box-clients"},"upload":1,"download":2,"start":"2026-11-25T10:00:00Z","chains":["DIRECT"]},
		{"id":"b","metadata":{"sourceIP":"192.168.1.11"},"upload":1,"download":2,"start":"2026-11-25T10:00:00Z","chains":["DIRECT"]}]}`))

	list, err := b.listDevices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byIP := map[string]Device{}
	for _, d := range list {
		byIP[d.IP] = d
	}
	mac := byIP["192.168.1.10"]
	if mac.Client == nil || mac.Client.Name != "小王的 MacBook" || mac.Client.Version != "0.2.22" || mac.Client.Online || mac.MAC == "" {
		t.Fatalf("macbook = %+v client = %+v", mac, mac.Client)
	}
	nas := byIP["192.168.1.11"]
	if nas.Client == nil || !nas.Client.Online || nas.Client.Entry != 1 || nas.Usage == nil || nas.Usage.Connections != 2 {
		t.Fatalf("nas = %+v client = %+v", nas, nas.Client)
	}
	if _, ok := byIP["10.8.0.5"]; ok {
		t.Fatal("8 天没来的客户端还在")
	}
	if _, ok := byIP["192.168.1.30"]; ok {
		t.Fatal("8 天没来的客户端还在")
	}

	// 跨网段的新客户端：单列一行，没有 MAC。
	b.clients.Seen(netip.MustParseAddr("10.8.0.6"))
	list, _ = b.listDevices(ctx)
	var cross *Device
	for i := range list {
		if list[i].IP == "10.8.0.6" {
			cross = &list[i]
		}
	}
	if cross == nil || cross.MAC != "" || cross.Client == nil || !cross.Online || !cross.Client.Online {
		t.Fatalf("cross = %+v", cross)
	}
}
