package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// ---- 出口检测 ----
//
// 登录账号和应用启动时，看这台电脑此刻的出口：请求 Cloudflare 的 /cdn-cgi/trace（中立站点），
// 再请求账号所属服务（Claude、ChatGPT，同样在 Cloudflare 后面）的 trace，读出出口 IP 和国家。
// Lite 版不做代理，请求走系统网络（环境变量里的代理除外，和命令行里的 claude / codex 一致）。
//
// 异常的情况（界面据此拦一下，用户可以确认后强制上号）：
//   - 任何一个站点探测失败（连不上，或被服务端拒绝）；
//   - 设置了「允许的出口国家」，而出口国家不在其中；
//   - 不同站点看到的出口国家不一致（分流或代理没配好）。

type egressSite struct {
	Name string
	URL  string
}

const cloudflareTrace = "https://www.cloudflare.com/cdn-cgi/trace"

// egressSites 按账号类型选检测站点：中立的 Cloudflare 一定检测，Claude、Codex 再加上自己的服务。
func egressSites(kind string) []egressSite {
	sites := []egressSite{{"Cloudflare", cloudflareTrace}}
	switch kind {
	case "claude":
		sites = append(sites, egressSite{"Claude", "https://claude.ai/cdn-cgi/trace"})
	case "codex":
		sites = append(sites, egressSite{"ChatGPT", "https://chatgpt.com/cdn-cgi/trace"})
	}
	return sites
}

// EgressResult 是一个站点看到的出口。
type EgressResult struct {
	Site      string `json:"site"`
	OK        bool   `json:"ok"`
	IP        string `json:"ip,omitempty"`
	Country   string `json:"country,omitempty"`
	Colo      string `json:"colo,omitempty"`
	Error     string `json:"error,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
}

// EgressReport 是一次检测的结果。
type EgressReport struct {
	Results   []EgressResult `json:"results"`
	Allowed   []string       `json:"allowed_countries"`
	Problems  []string       `json:"problems"`
	Abnormal  bool           `json:"abnormal"`
	CheckedAt time.Time      `json:"checked_at"`
}

func checkEgress(ctx context.Context, kind string, allowed []string) EgressReport {
	sites := egressSites(kind)
	results := make([]EgressResult, len(sites))
	var wg sync.WaitGroup
	for i, s := range sites {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = probeEgress(ctx, s)
		}()
	}
	wg.Wait()
	rep := EgressReport{Results: results, Allowed: allowed, Problems: judgeEgress(results, allowed), CheckedAt: time.Now().UTC()}
	rep.Abnormal = len(rep.Problems) > 0
	return rep
}

// judgeEgress 列出异常的原因，没有异常时返回空。
func judgeEgress(results []EgressResult, allowed []string) []string {
	problems := []string{}
	countries := map[string]bool{}
	for _, r := range results {
		if !r.OK {
			problems = append(problems, fmt.Sprintf("%s 检测失败：%s", r.Site, r.Error))
			continue
		}
		if r.Country != "" {
			countries[r.Country] = true
		}
		if len(allowed) > 0 && r.Country != "" && !slices.Contains(allowed, r.Country) {
			problems = append(problems, fmt.Sprintf("%s 看到的出口在 %s（%s），不在允许的国家 %s 里", r.Site, r.Country, r.IP, strings.Join(allowed, "、")))
		}
	}
	if len(countries) > 1 {
		var list []string
		for c := range countries {
			list = append(list, c)
		}
		slices.Sort(list)
		problems = append(problems, "不同站点看到的出口国家不一致："+strings.Join(list, "、"))
	}
	return problems
}

func probeEgress(ctx context.Context, s egressSite) EgressResult {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	start := time.Now()
	res := EgressResult{Site: s.Name}
	fail := func(err error) EgressResult {
		var ue interface{ Unwrap() error }
		if errors.As(err, &ue) && ue.Unwrap() != nil {
			err = ue.Unwrap()
		}
		res.Error = err.Error()
		res.LatencyMS = time.Since(start).Milliseconds()
		return res
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return fail(err)
	}
	req.Header.Set("User-Agent", "banbu-connect-lite")
	client := &http.Client{
		Transport:     &http.Transport{Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true, TLSHandshakeTimeout: 5 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("HTTP %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return fail(err)
	}
	ip, country, colo, ok := parseTrace(string(body))
	if !ok {
		return fail(errors.New("返回内容里没有 IP"))
	}
	res.OK, res.IP, res.Country, res.Colo = true, ip, country, colo
	res.LatencyMS = time.Since(start).Milliseconds()
	return res
}

// parseTrace 解析 Cloudflare /cdn-cgi/trace 的 ip=、loc=、colo= 行。
func parseTrace(body string) (ip, country, colo string, ok bool) {
	for _, line := range strings.Split(body, "\n") {
		k, v, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch k {
		case "ip":
			ip = v
		case "loc":
			country = strings.ToUpper(v)
		case "colo":
			colo = v
		}
	}
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return "", "", "", false
	}
	return a.Unmap().String(), country, colo, true
}

// ---- 设置：允许的出口国家 ----

type egressSettings struct {
	AllowedCountries []string `json:"allowed_countries"`
}

type egressStore struct {
	mu   sync.Mutex
	path string
}

func (s *egressStore) get() egressSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	var v egressSettings
	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, &v)
	}
	if v.AllowedCountries == nil {
		v.AllowedCountries = []string{}
	}
	return v
}

func (s *egressStore) set(v egressSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o600)
}

// normalizeCountries 只留两位字母的国家代码，转大写、去重。
func normalizeCountries(in []string) []string {
	out := []string{}
	for _, c := range in {
		c = strings.ToUpper(strings.TrimSpace(c))
		if len(c) != 2 || c[0] < 'A' || c[0] > 'Z' || c[1] < 'A' || c[1] > 'Z' || slices.Contains(out, c) {
			continue
		}
		out = append(out, c)
		if len(out) >= 20 {
			break
		}
	}
	return out
}

func egressRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, dataDir string) {
	store := &egressStore{path: filepath.Join(dataDir, "egress.json")}
	// kind=claude|codex|gemini：决定除 Cloudflare 外再检测哪个服务。
	mux.Handle("POST /api/v1/desktop/egress/check", authed(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		writeJSON(w, http.StatusOK, checkEgress(ctx, r.URL.Query().Get("kind"), store.get().AllowedCountries))
	}))
	mux.Handle("GET /api/v1/desktop/egress/settings", authed(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, store.get())
	}))
	mux.Handle("PUT /api/v1/desktop/egress/settings", authed(func(w http.ResponseWriter, r *http.Request) {
		var in egressSettings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, apiErr{"bad_request", "请求内容无效"})
			return
		}
		v := egressSettings{AllowedCountries: normalizeCountries(in.AllowedCountries)}
		if err := store.set(v); err != nil {
			writeJSON(w, http.StatusInternalServerError, apiErr{"save_failed", "保存失败：" + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, v)
	}))
}
