package trust

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Config 是受信网络的设置，保存在数据目录的 network.json。
type Config struct {
	// TrustedSSIDs 是受信的 Wi-Fi 名称，区分大小写。在这些 Wi-Fi 上、出口符合要求时直连放行。
	// 为空表示不启用：受保护的服务始终经本机内核。
	TrustedSSIDs []string `json:"trusted_ssids"`
	// ExpectedEgress 是允许的出口 IP 或网段。为空时，每个 Wi-Fi 第一次探测到的出口被记为基准，之后出口变化即阻断。
	ExpectedEgress []string `json:"expected_egress"`
	// AllowedCountries 是允许的出口地区（ISO 3166 两位代码，如 US、JP）；为空不检查。
	AllowedCountries []string `json:"allowed_countries"`
	// ProbeURLs 依次尝试，取第一个成功的结果。
	ProbeURLs []string `json:"probe_urls"`
	// Baselines 是每个 Wi-Fi 记住的出口 IP（ExpectedEgress 为空时使用），由程序维护。
	Baselines map[string]string `json:"baselines"`
	// AutoStartProxy 为真时，从受信的 Wi-Fi 切换到其他网络（其他 Wi-Fi、有线等）后，内核没在运行就自动启动它。
	// 缺省开启（新装和没有这个字段的老配置）；用户保存过的设置里有这个字段，按保存的值。
	AutoStartProxy bool `json:"auto_start_proxy"`
	// GateDirect 为真时，本机内核一直运行、TUN 接管，受信网络上受保护服务的连接由出口闸门逐个确认出口后
	// 经内核的 DIRECT 交给路由盒，不符合时经代理组。缺省开启（同 AutoStartProxy）。
	GateDirect bool `json:"gate_direct"`
}

// 默认探测 chatgpt.com 的 Cloudflare trace：看到的是受保护服务那一侧的出口，
// 而且 ChatGPT 没有 IP 层规则，阻断期间探测本身不会被拦。
// claude.ai 在 Claude 的网段里，阻断期间会被 IP 层拦住，不能用作唯一的探测地址。
var defaultProbeURLs = []string{"https://chatgpt.com/cdn-cgi/trace"}

// DefaultHoldListen 是阻断入口的地址。macOS 上 /etc/resolver 只能用 127.0.0.1；
// Windows 的 NRPT 不能指定端口，127.0.0.2:53 已经给了 Mihomo，所以用 127.0.0.3。
func DefaultHoldListen() string {
	if runtime.GOOS == "windows" {
		return "127.0.0.3:53"
	}
	return "127.0.0.1:10053"
}

func defaultConfig() Config {
	return Config{
		TrustedSSIDs:     []string{},
		ExpectedEgress:   []string{},
		AllowedCountries: []string{},
		ProbeURLs:        append([]string(nil), defaultProbeURLs...),
		Baselines:        map[string]string{},
		AutoStartProxy:   true,
		GateDirect:       true,
	}
}

func (c *Config) normalize() error {
	ssids := []string{}
	for _, s := range c.TrustedSSIDs {
		// SSID 首尾可以有空格，但几乎都是误输入，按去空格处理。
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if len(s) > 32 {
			return fmt.Errorf("Wi-Fi 名称超过 32 字节：%s", s)
		}
		ssids = append(ssids, s)
	}
	c.TrustedSSIDs = unique(ssids)

	egress := []string{}
	for _, raw := range c.ExpectedEgress {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		p, err := parsePrefix(v)
		if err != nil {
			return fmt.Errorf("出口 IP 无效：%s", raw)
		}
		egress = append(egress, p.String())
	}
	c.ExpectedEgress = unique(egress)

	countries := []string{}
	for _, raw := range c.AllowedCountries {
		v := strings.ToUpper(strings.TrimSpace(raw))
		if v == "" {
			continue
		}
		if len(v) != 2 || v[0] < 'A' || v[0] > 'Z' || v[1] < 'A' || v[1] > 'Z' {
			return fmt.Errorf("地区须为两位字母代码，如 US：%s", raw)
		}
		countries = append(countries, v)
	}
	c.AllowedCountries = unique(countries)

	probes := []string{}
	for _, raw := range c.ProbeURLs {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		u, err := url.Parse(v)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("探测地址须为 https 地址：%s", raw)
		}
		probes = append(probes, u.String())
	}
	if len(probes) == 0 {
		probes = append(probes, defaultProbeURLs...)
	}
	c.ProbeURLs = unique(probes)

	if c.Baselines == nil {
		c.Baselines = map[string]string{}
	}
	for ssid, ip := range c.Baselines {
		if _, err := netip.ParseAddr(ip); err != nil {
			delete(c.Baselines, ssid)
		}
	}

	return nil
}

// checkProbesNotBlocked 拒绝落在受保护网段里的 IP 探测地址：阻断期间探测不通，就永远恢复不了。
// 域名形式的地址无法提前判断，探测失败时界面会提示。
func (c Config) checkProbesNotBlocked(cidrs []string) error {
	var prefixes []netip.Prefix
	for _, raw := range cidrs {
		if p, err := netip.ParsePrefix(raw); err == nil {
			prefixes = append(prefixes, p)
		}
	}
	for _, raw := range c.ProbeURLs {
		u, _ := url.Parse(raw)
		addr, err := netip.ParseAddr(strings.Trim(u.Hostname(), "[]"))
		if err != nil {
			continue
		}
		for _, p := range prefixes {
			if p.Contains(addr) {
				return fmt.Errorf("探测地址 %s 在受保护的网段 %s 里，阻断期间无法探测", raw, p)
			}
		}
	}
	return nil
}

// parsePrefix 接受单个 IP 或网段，单个 IP 按 /32、/128 处理。
func parsePrefix(v string) (netip.Prefix, error) {
	if strings.Contains(v, "/") {
		p, err := netip.ParsePrefix(v)
		if err != nil {
			return netip.Prefix{}, err
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(v)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

func unique(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func cloneConfig(c Config) Config {
	c.TrustedSSIDs = append([]string{}, c.TrustedSSIDs...)
	c.ExpectedEgress = append([]string{}, c.ExpectedEgress...)
	c.AllowedCountries = append([]string{}, c.AllowedCountries...)
	c.ProbeURLs = append([]string{}, c.ProbeURLs...)
	b := make(map[string]string, len(c.Baselines))
	for k, v := range c.Baselines {
		b[k] = v
	}
	c.Baselines = b
	return c
}

func loadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return defaultConfig(), nil
	}
	if err != nil {
		return Config{}, err
	}
	c := defaultConfig()
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("配置文件损坏：%w", err)
	}
	if err := c.normalize(); err != nil {
		return Config{}, fmt.Errorf("配置文件无效：%w", err)
	}
	return c, nil
}

func saveConfig(path string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// DefaultProbeURLs 返回默认的出口探测地址（路由盒没有受信网络配置，直接用它）。
func DefaultProbeURLs() []string { return append([]string(nil), defaultProbeURLs...) }
