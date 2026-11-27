package box

import (
	"net/netip"
	"sync"
	"time"

	"boxctl/internal/boxfind"
)

// 接入的 Banbu Connect 客户端（设备页）：客户端每 30 秒来发现端口复查一次路由盒（ADR-073），
// 验证通过后自报名字、系统和版本（boxfind.Hello）。这里按来源 IP 记在内存里，面板重启后由下一次复查补上；
// 只用于显示，不参与任何放行判断。旧版客户端只有复查，没有自报，显示为「Banbu Connect」。

const (
	// clientOnline：多久内来过算在线。客户端 30 秒复查一次，留两次的余量。
	clientOnline = 90 * time.Second
	// clientForget：多久没来就不再显示。
	clientForget = 7 * 24 * time.Hour
	// maxClients 是记录的上限，满了丢掉最久没来的（发现端口只在 LAN 上，正常远到不了这个数）。
	maxClients = 512
)

// BoxClient 是一台接入的客户端，设备页显示用。
type BoxClient struct {
	Name     string    `json:"name,omitempty"`    // 电脑名（客户端自报）
	OS       string    `json:"os,omitempty"`      // 系统和版本
	Version  string    `json:"version,omitempty"` // Banbu Connect 版本
	LastSeen time.Time `json:"last_seen"`         // 最近一次来复查路由盒
	Online   bool      `json:"online"`            // clientOnline 内来过，或者现在有经代理入口的连接
	Entry    int       `json:"entry"`             // 现在经代理入口的连接数
}

type clientRegistry struct {
	mu  sync.Mutex
	m   map[netip.Addr]*BoxClient
	now func() time.Time
}

func newClientRegistry() *clientRegistry {
	return &clientRegistry{m: map[netip.Addr]*BoxClient{}, now: time.Now}
}

func (r *clientRegistry) get(ip netip.Addr) *BoxClient {
	c := r.m[ip]
	if c == nil {
		if len(r.m) >= maxClients {
			var oldest netip.Addr
			for k, v := range r.m {
				if !oldest.IsValid() || v.LastSeen.Before(r.m[oldest].LastSeen) {
					oldest = k
				}
			}
			delete(r.m, oldest)
		}
		c = &BoxClient{}
		r.m[ip] = c
	}
	return c
}

// Seen 实现 boxfind.Recorder：一次挑战应答。
func (r *clientRegistry) Seen(ip netip.Addr) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.get(ip).LastSeen = r.now()
}

// Hello 实现 boxfind.Recorder：客户端自报。同一个 IP 换了电脑（DHCP 重新分配）时以最新的为准。
func (r *clientRegistry) Hello(ip netip.Addr, h boxfind.Hello) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.get(ip)
	c.Name, c.OS, c.Version, c.LastSeen = h.Name, h.OS, h.Version, r.now()
}

// snapshot 返回按 IP 的客户端，去掉太久没来的。entry 是各来源 IP 现在经代理入口的连接数：
// 有连接但没记录的（面板刚重启、客户端还没来复查）也算接入的客户端。
func (r *clientRegistry) snapshot(entry map[string]int) map[string]BoxClient {
	out := map[string]BoxClient{}
	if r != nil {
		r.collect(out)
	}
	for ip, n := range entry {
		if n == 0 {
			continue
		}
		v := out[ip]
		v.Entry, v.Online = n, true
		out[ip] = v
	}
	return out
}

func (r *clientRegistry) collect(out map[string]BoxClient) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for ip, c := range r.m {
		if now.Sub(c.LastSeen) > clientForget {
			delete(r.m, ip)
			continue
		}
		v := *c
		v.Online = now.Sub(c.LastSeen) <= clientOnline
		out[ip.String()] = v
	}
}
