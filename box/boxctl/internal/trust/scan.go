package trust

import (
	"context"
	"net/url"
	"sync"
	"time"
)

// 出口检测：参照 ippure 的「IP 出口分布」，对一组站点分别请求 /cdn-cgi/trace，
// 看每个站点、每条路看到的出口 IP、地区和 Cloudflare 接入点。分流的网络（路由盒、按域名选线路）里，
// 同一时刻不同站点的出口可能不同，只看一个地址会漏掉问题。

// ScanSite 是一个检测目标。
type ScanSite struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Protected 表示这是受保护服务自己的地址。
	Protected bool `json:"protected"`
}

// ScanPath 是一条检测路径。
type ScanPath struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Hint  string `json:"hint,omitempty"`
	proxy *url.URL
	net   string
}

// NewScanPath 创建一条路径：proxy 为空时由本机直接请求，network 可以限定 tcp4、tcp6。
func NewScanPath(key, label, hint string, proxy *url.URL, network string) ScanPath {
	return ScanPath{Key: key, Label: label, Hint: hint, proxy: proxy, net: network}
}

type ScanResult struct {
	Site   string `json:"site"`
	Path   string `json:"path"`
	Egress Egress `json:"egress"`
}

type ScanReport struct {
	Sites     []ScanSite   `json:"sites"`
	Paths     []ScanPath   `json:"paths"`
	Results   []ScanResult `json:"results"`
	CheckedAt time.Time    `json:"checked_at"`
}

// DefaultScanSites 是默认的检测站点：中立的 Cloudflare，加上常见的 AI 服务（都在 Cloudflare 后面）。
func DefaultScanSites() []ScanSite {
	return []ScanSite{
		{Name: "Cloudflare", URL: "https://www.cloudflare.com/cdn-cgi/trace"},
		{Name: "ChatGPT", URL: "https://chatgpt.com/cdn-cgi/trace"},
		{Name: "OpenAI", URL: "https://openai.com/cdn-cgi/trace"},
		{Name: "Sora", URL: "https://sora.com/cdn-cgi/trace"},
		{Name: "Claude", URL: "https://claude.ai/cdn-cgi/trace"},
		{Name: "Anthropic", URL: "https://www.anthropic.com/cdn-cgi/trace"},
		{Name: "Perplexity", URL: "https://perplexity.ai/cdn-cgi/trace"},
	}
}

// Scan 并行检测所有站点 × 路径。prober 为测试替换用，nil 时用 HTTPProber。
func Scan(ctx context.Context, sites []ScanSite, paths []ScanPath, prober func(ScanPath) EgressProber) ScanReport {
	if prober == nil {
		prober = func(p ScanPath) EgressProber { return HTTPProber{Proxy: p.proxy, Network: p.net} }
	}
	r := ScanReport{Sites: sites, Paths: paths, Results: make([]ScanResult, 0, len(sites)*len(paths))}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for _, s := range sites {
		for _, p := range paths {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				eg := prober(p).Probe(ctx, []string{s.URL})
				mu.Lock()
				r.Results = append(r.Results, ScanResult{Site: s.Name, Path: p.Key, Egress: eg})
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	r.CheckedAt = time.Now().UTC()
	return r
}
