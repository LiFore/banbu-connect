package killswitch

// Path 是受保护的服务当前应走的路，由 Policy 根据网络决定，Engine 据此写规则。
type Path string

const (
	// PathProxy 经本机内核：受保护的域名只交给 Mihomo 解析，网段在物理网卡上拦截。内核不可用时断开。
	PathProxy Path = "proxy"
	// PathDirect 在受信网络上直连：不写任何规则，代理由网络里的路由盒负责。
	PathDirect Path = "direct"
	// PathHold 阻断：受保护的域名交给阻断入口（一律拒绝），网段在物理网卡上拦截。
	PathHold Path = "hold"
)

// Verdict 是一次判断：走哪条路，以及原因（展示给用户）。
type Verdict struct {
	Path   Path   `json:"path"`
	Reason string `json:"reason"`
	// Pending 表示阻断只是因为结果还没出来（正在探测出口），很快会有结论；只对 PathHold 有意义。
	Pending bool `json:"pending,omitempty"`
}

// Policy 根据当前网络给出判断。tunActive 表示流量已经被本机内核的 TUN 接管：
// 这时受保护的流量本来就经过内核，受信网络的判断不再适用。
//
// 实现必须只读内存状态、很快返回：Engine 在每次健康检查后调用它。
type Policy interface {
	Verdict(tunActive bool) Verdict
}

// proxyOnly 是没有设置 Policy 时的行为：始终经本机内核。
type proxyOnly struct{}

func (proxyOnly) Verdict(bool) Verdict { return Verdict{Path: PathProxy} }

// desiredFor 计算某条路对应的系统规则。直连不写规则，返回空。
func (c Config) desiredFor(p Path, holdListen string, interfaces []string) Desired {
	switch p {
	case PathDirect:
		return Desired{Interfaces: c.desired(interfaces).Interfaces}
	case PathHold:
		c.DNSListen = holdListen
	}
	return c.desired(interfaces)
}

// pathEvent 是切换到某条路时记录的事件。
func pathEvent(v Verdict) (level, msg string) {
	switch v.Path {
	case PathDirect:
		return "info", "受信网络，直连放行：" + v.Reason
	case PathHold:
		return "warn", "已阻断：" + v.Reason
	default:
		if v.Reason == "" {
			return "info", "经本机内核访问受保护的服务"
		}
		return "info", "经本机内核访问受保护的服务：" + v.Reason
	}
}
