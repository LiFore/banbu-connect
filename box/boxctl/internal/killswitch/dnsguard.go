package killswitch

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// DNSGuard 在 TUN 接管时，于物理网卡上拦截发往 53、853 端口的流量（防 DNS 泄漏）：
//
//   - 路由器、局域网 DNS、IPv6 链路本地 DNS 的路由比 TUN 更具体，查询直接走物理网卡；
//   - macOS 上指定网卡的（scoped）查询不经过路由表；
//   - Windows 上 Kill Switch 启用时关闭了 strict-route，系统会并行查询各网卡的 DNS。
//
// 内核的上游都是 443/853 加 IP 且经代理组，国内 DoH 走 443，不受影响。
// 与 Kill Switch 的规则不同，它不能持久：内核不在时必须撤销，否则整机无法解析域名。
// 服务启动时总是先撤销，再按 TUN 状态决定。
//
// TODO(verify:V29) 实机确认：开启后 tcpdump 物理网卡上没有 53 端口的包，泄漏测试只剩代理一侧的解析器；
// 强制门户、局域网域名在 TUN 接管时无法解析（预期如此，可在设置里关闭）。
type DNSGuard interface {
	Apply(ctx context.Context, interfaces []string) error
	Remove(ctx context.Context) error
	Active(ctx context.Context) (bool, error)
}

// GuardStatus 是 DNS 防泄漏的状态，展示用。
type GuardStatus struct {
	Enabled bool `json:"enabled"` // 设置里打开了
	Active  bool `json:"active"`  // 系统上的规则已生效
	// Delegated 表示在受信网络（路由盒）上，DNS 交给路由盒，不拦截。路由盒的 DNS 本身防泄漏。
	Delegated bool   `json:"delegated,omitempty"`
	Error     string `json:"error,omitempty"`
}

var guardPorts = []int{53, 853}

// LocalDNSSrcPorts 是内网域名转发口（ADR-080）发查询用的本机源端口，DNS 防泄漏放行从这些端口发出的 53 查询：
// 不受信网络上内网域名也要问当前网络的 DNS，其他程序直接问 DNS 仍然拦截。
var LocalDNSSrcPorts = [2]int{19184, 19191}

// guardPFRules 生成 macOS 的 pf 规则。
func guardPFRules(ifaces []string) string {
	ports := make([]string, len(guardPorts))
	for i, p := range guardPorts {
		ports[i] = fmt.Sprint(p)
	}
	on := strings.Join(ifaces, " ")
	return fmt.Sprintf("pass out quick on { %s } proto udp from any port %d:%d to any port 53\n", on, LocalDNSSrcPorts[0], LocalDNSSrcPorts[1]) +
		fmt.Sprintf("block drop out quick on { %s } proto { tcp, udp } from any to any port { %s }\n", on, strings.Join(ports, ", "))
}

// FakeGuard 是测试和预览用的假实现。
type FakeGuard struct {
	mu     sync.Mutex
	On     bool
	Ifaces []string
	Fail   error
	Calls  int
}

func (f *FakeGuard) Apply(_ context.Context, ifaces []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	if f.Fail != nil {
		return f.Fail
	}
	f.On, f.Ifaces = true, slices.Clone(ifaces)
	return nil
}

func (f *FakeGuard) Remove(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	f.On, f.Ifaces = false, nil
	return nil
}

func (f *FakeGuard) Active(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.On, nil
}
