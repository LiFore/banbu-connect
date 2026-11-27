package desktop

import (
	"testing"

	"boxctl/internal/core"
	"boxctl/internal/dist"
	"boxctl/internal/killswitch"
	"boxctl/internal/trust"
)

func TestAccessMode(t *testing.T) {
	running := core.CoreStatus{Running: true, TUNActive: true}
	stopped := core.CoreStatus{}
	ks := func(p killswitch.Path) killswitch.Status {
		return killswitch.Status{Enabled: true, Verdict: killswitch.Verdict{Path: p}}
	}
	boxNet := trust.Status{Trusted: true, Box: trust.BoxStatus{ID: "home-box", Addr: "192.168.10.100"}}
	boxGate := boxNet
	boxGate.Gate = trust.GateStatus{Enabled: true, Active: true}
	wifi := trust.Status{Trusted: true}
	other := trust.Status{}

	for _, c := range []struct {
		name string
		core core.CoreStatus
		ks   killswitch.Status
		n    trust.Status
		want string
	}{
		{"阻断", running, ks(killswitch.PathHold), other, dist.AccessHold},
		{"路由盒直连", stopped, ks(killswitch.PathDirect), boxNet, dist.AccessBox},
		{"路由盒闸门直连", running, ks(killswitch.PathProxy), boxGate, dist.AccessBox},
		{"受信 Wi-Fi", stopped, ks(killswitch.PathDirect), wifi, dist.AccessTrusted},
		{"路由盒网络但 TUN 全接管", running, ks(killswitch.PathProxy), boxNet, dist.AccessLocal},
		{"本机代理", running, ks(killswitch.PathProxy), other, dist.AccessLocal},
		{"未使用", stopped, killswitch.Status{}, other, dist.AccessOff},
	} {
		if got := accessMode(c.core, c.ks, c.n); got != c.want {
			t.Errorf("%s：%s，想要 %s", c.name, got, c.want)
		}
	}

	st := accessState(running, killswitch.Status{Verdict: killswitch.Verdict{Reason: "在受信 Wi-Fi「Home」上"}}, wifi)
	if st.Reason != "在受信 Wi-Fi「…」上" {
		t.Fatalf("原因里的 Wi-Fi 名称应去掉：%q", st.Reason)
	}
}
