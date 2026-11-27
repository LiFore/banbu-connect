package killswitch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// Service 是一个受保护的服务。Group 是 Mihomo 里承载它的严格代理组，用于校验和生成规则片段。
type Service struct {
	Name     string   `json:"name"`
	Enabled  bool     `json:"enabled"`
	Group    string   `json:"group"`
	Suffixes []string `json:"suffixes"`
	CIDRs    []string `json:"cidrs"`
}

type Config struct {
	Enabled          bool      `json:"enabled"`
	Services         []Service `json:"services"`
	DNSListen        string    `json:"dns_listen"`
	Controller       string    `json:"controller"`
	ControllerSecret string    `json:"controller_secret,omitempty"`
}

// defaultConfig 的 Claude、ChatGPT 须与 client/guard/protected.json 一致（有测试保证）。
// CIDR 只放官方公布的固定网段；ChatGPT 共用 Cloudflare，只有 DNS 层。
func defaultConfig() Config {
	listen := "127.0.0.1:1053"
	if runtime.GOOS == "windows" {
		listen = "127.0.0.2:53" // NRPT 不能指定端口
	}
	return Config{
		DNSListen: listen,
		Services: []Service{
			{
				Name: "Claude", Enabled: true, Group: "Claude",
				Suffixes: []string{"claude.ai", "claude.com", "clau.de", "anthropic.com", "claudeusercontent.com", "claudemcpclient.com"},
				CIDRs:    []string{"160.79.104.0/23", "153.61.224.0/22", "2607:6bc0::/48"},
			},
			{
				Name: "ChatGPT", Enabled: true, Group: "ChatGPT",
				Suffixes: []string{"chatgpt.com", "openai.com", "oaistatic.com", "oaiusercontent.com"},
				CIDRs:    []string{},
			},
		},
	}
}

var suffixRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9-]{2,63}$`)

// normalize 校验并规范化。这些值最终进入系统 DNS 和防火墙配置，所以必须严格。
func (c *Config) normalize() error {
	seen := map[string]bool{}
	for i := range c.Services {
		s := &c.Services[i]
		s.Name = strings.TrimSpace(s.Name)
		s.Group = strings.TrimSpace(s.Group)
		if s.Name == "" || len([]rune(s.Name)) > 64 {
			return fmt.Errorf("服务名称须为 1–64 字")
		}
		if seen[s.Name] {
			return fmt.Errorf("服务名称重复：%s", s.Name)
		}
		seen[s.Name] = true

		suffixes := []string{}
		for _, raw := range s.Suffixes {
			v := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(raw)), ".")
			if v == "" {
				continue
			}
			if !suffixRE.MatchString(v) {
				return fmt.Errorf("%s：域名后缀无效：%s", s.Name, raw)
			}
			suffixes = append(suffixes, v)
		}
		s.Suffixes = uniqueKeepOrder(suffixes)

		cidrs := []string{}
		for _, raw := range s.CIDRs {
			v := strings.TrimSpace(raw)
			if v == "" {
				continue
			}
			p, err := netip.ParsePrefix(v)
			if err != nil {
				return fmt.Errorf("%s：网段无效：%s", s.Name, raw)
			}
			// 过宽的网段一旦写错，内核故障时会断掉大片无关网站。
			if (p.Addr().Is4() && p.Bits() < 8) || (p.Addr().Is6() && p.Bits() < 16) {
				return fmt.Errorf("%s：网段过宽：%s", s.Name, raw)
			}
			cidrs = append(cidrs, p.Masked().String())
		}
		s.CIDRs = uniqueKeepOrder(cidrs)
	}
	listen, err := netip.ParseAddrPort(strings.TrimSpace(c.DNSListen))
	if err != nil || !listen.Addr().IsLoopback() {
		return fmt.Errorf("DNS 入口须为本机回环地址和端口，如 127.0.0.1:1053")
	}
	c.DNSListen = listen.String()
	c.Controller = strings.TrimRight(strings.TrimSpace(c.Controller), "/")
	if c.Controller != "" && !strings.HasPrefix(c.Controller, "http://127.") && !strings.HasPrefix(c.Controller, "http://localhost:") {
		return fmt.Errorf("Mihomo 控制接口须为本机地址，如 http://127.0.0.1:9097")
	}
	return nil
}

func uniqueKeepOrder(in []string) []string {
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

// desired 计算启用的服务应落到系统上的规则。
func (c Config) desired(interfaces []string) Desired {
	var suffixes, cidrs []string
	for _, s := range c.Services {
		if s.Enabled {
			suffixes = append(suffixes, s.Suffixes...)
			cidrs = append(cidrs, s.CIDRs...)
		}
	}
	suffixes = uniqueKeepOrder(suffixes)
	cidrs = uniqueKeepOrder(cidrs)
	ifaces := uniqueKeepOrder(interfaces)
	sort.Strings(suffixes)
	sort.Strings(cidrs)
	sort.Strings(ifaces)
	ip, port := "", 0
	if ap, err := netip.ParseAddrPort(c.DNSListen); err == nil {
		ip, port = ap.Addr().String(), int(ap.Port())
	}
	return Desired{Suffixes: suffixes, CIDRs: cidrs, ListenIP: ip, ListenPort: port, Interfaces: ifaces}
}

func loadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return defaultConfig(), nil
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
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
	return writeFileAtomic(path, b, 0o600)
}

func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// DefaultServices 返回默认的受保护服务（路由盒与桌面共用）。
func DefaultServices() []Service { return defaultConfig().Services }

// NormalizeServices 校验并规范化受保护服务（路由盒的 Kill Switch 配置用）。
func NormalizeServices(services []Service) ([]Service, error) {
	c := Config{Services: services, DNSListen: "127.0.0.1:1053"}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	if c.Services == nil {
		c.Services = []Service{}
	}
	return c.Services, nil
}
