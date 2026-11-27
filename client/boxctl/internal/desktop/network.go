package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"

	"boxctl/internal/killswitch"
	"boxctl/internal/trust"
)

// networkView 是受信网络设置在 API 上的形式。基准只能删除，由程序记录。
type networkView struct {
	TrustedSSIDs     []string          `json:"trusted_ssids"`
	ExpectedEgress   []string          `json:"expected_egress"`
	AllowedCountries []string          `json:"allowed_countries"`
	ProbeURLs        []string          `json:"probe_urls"`
	Baselines        map[string]string `json:"baselines"`
	AutoStartProxy   bool              `json:"auto_start_proxy"`
	GateDirect       bool              `json:"gate_direct"`
}

func viewOfNetwork(c trust.Config) networkView {
	return networkView{c.TrustedSSIDs, c.ExpectedEgress, c.AllowedCountries, c.ProbeURLs, c.Baselines, c.AutoStartProxy, c.GateDirect}
}

func networkRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, ks *killswitch.Engine, n *trust.Evaluator) {
	fail := func(w http.ResponseWriter, err error) {
		var invalid *trust.ValidationError
		if errors.As(err, &invalid) {
			writeJSON(w, http.StatusBadRequest, apiErr{"invalid", invalid.Msg})
			return
		}
		writeJSON(w, http.StatusInternalServerError, apiErr{"failed", err.Error()})
	}
	mux.Handle("GET /api/v1/desktop/network/config", authed(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, viewOfNetwork(n.Config()))
	}))
	mux.Handle("PUT /api/v1/desktop/network/config", authed(func(w http.ResponseWriter, r *http.Request) {
		var in networkView
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, apiErr{"bad_request", "请求内容无效"})
			return
		}
		next := trust.Config{TrustedSSIDs: in.TrustedSSIDs, ExpectedEgress: in.ExpectedEgress,
			AllowedCountries: in.AllowedCountries, ProbeURLs: in.ProbeURLs, Baselines: in.Baselines,
			AutoStartProxy: in.AutoStartProxy, GateDirect: in.GateDirect}
		if err := n.UpdateConfig(next, blockedCIDRs(ks.Config())); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, viewOfNetwork(n.Config()))
	}))
	action := func(path string, f func() error) {
		mux.Handle("POST "+path, authed(func(w http.ResponseWriter, _ *http.Request) {
			if err := f(); err != nil {
				fail(w, err)
				return
			}
			writeJSON(w, http.StatusOK, n.Status())
		}))
	}
	action("/api/v1/desktop/network/trust-current", n.TrustCurrent)
	action("/api/v1/desktop/network/accept-egress", n.AcceptEgress)
	action("/api/v1/desktop/network/recheck", func() error { n.Recheck(); return nil })
	action("/api/v1/desktop/network/box-recheck", func() error { n.RecheckBox(); return nil })
}

func blockedCIDRs(c killswitch.Config) []string {
	var out []string
	for _, s := range c.Services {
		if s.Enabled {
			out = append(out, s.CIDRs...)
		}
	}
	return out
}

// fakeDetector 把当前出口当作指定的 Wi-Fi，预览用。
type fakeDetector struct{ ssid string }

func (f fakeDetector) Detect(context.Context) trust.Network {
	n := trust.Network{Interface: "en0", WiFi: true, SSID: f.ssid}
	if c, err := net.Dial("udp4", "1.1.1.1:443"); err == nil {
		c.Close()
	} else {
		n = trust.Network{}
	}
	return n
}
