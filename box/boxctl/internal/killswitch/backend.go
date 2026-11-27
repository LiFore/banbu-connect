package killswitch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// Tag 标记本工具写入系统的规则。与 client/guard 脚本相同，所以能接管它们装的规则。
const Tag = "boxguard"

// Desired 是应落到系统上的规则，所有列表已排序去重。
type Desired struct {
	Suffixes   []string `json:"suffixes"`
	CIDRs      []string `json:"cidrs"`
	ListenIP   string   `json:"listen_ip"`
	ListenPort int      `json:"listen_port"`
	Interfaces []string `json:"interfaces"`
}

// Actual 是系统上规则与 Desired 的比较结果。
type Actual struct {
	Installed bool     `json:"installed"`
	InSync    bool     `json:"in_sync"`
	Diffs     []string `json:"diffs"`
	Warnings  []string `json:"warnings"`
}

// Backend 操作系统规则。规则写在系统里且持久生效，不依赖本进程存活。
type Backend interface {
	Interfaces(ctx context.Context) ([]string, error)
	Inspect(ctx context.Context, d Desired) (Actual, error)
	Apply(ctx context.Context, d Desired) error
	Remove(ctx context.Context) error
	FlushDNS(ctx context.Context) error
	ValidateListen(ip string, port int) error
}

// ---- 平台无关的规则生成，便于在任意平台测试 ----

const resolverTag = "# " + Tag

func resolverContent(ip string, port int) string {
	return fmt.Sprintf("%s\nnameserver %s\nport %d\n", resolverTag, ip, port)
}

// pfRules 生成 pf 规则：只作用于物理网卡，走 TUN 的流量不受影响。
func pfRules(d Desired) string {
	var v4, v6 []string
	for _, c := range d.CIDRs {
		if strings.Contains(c, ":") {
			v6 = append(v6, c)
		} else {
			v4 = append(v4, c)
		}
	}
	ifaces := strings.Join(d.Interfaces, " ")
	var b strings.Builder
	if len(v4) > 0 {
		fmt.Fprintf(&b, "block drop out quick on { %s } inet from any to { %s }\n", ifaces, strings.Join(v4, " "))
	}
	if len(v6) > 0 {
		fmt.Fprintf(&b, "block drop out quick on { %s } inet6 from any to { %s }\n", ifaces, strings.Join(v6, " "))
	}
	return b.String()
}

// parseHardwarePorts 从 networksetup -listallhardwareports 取 en* 网卡。
func parseHardwarePorts(out string) []string {
	var ifaces []string
	for _, line := range strings.Split(out, "\n") {
		name, ok := strings.CutPrefix(strings.TrimSpace(line), "Device: ")
		if ok && strings.HasPrefix(name, "en") {
			ifaces = append(ifaces, name)
		}
	}
	return ifaces
}

// nrptNamespaces 生成 NRPT 命名空间：根域名和子域名要分别列出。
func nrptNamespaces(suffixes []string) []string {
	out := make([]string, 0, 2*len(suffixes))
	for _, s := range suffixes {
		out = append(out, s, "."+s)
	}
	slices.Sort(out)
	return out
}

// firewallHash 写进防火墙规则的描述，用于发现网段或网卡的变化。
func firewallHash(d Desired) string {
	sum := sha256.Sum256([]byte(strings.Join(d.CIDRs, ",") + "|" + strings.Join(d.Interfaces, ",")))
	return hex.EncodeToString(sum[:])
}

// ---- 假后端：测试和 -fake 预览用，不碰系统 ----

type FakeBackend struct {
	mu        sync.Mutex
	Ifaces    []string
	Current   *Desired
	FailApply error
	Warnings  []string
	Applied   int
	Removed   int
	Flushed   int
}

func (f *FakeBackend) Interfaces(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.Ifaces), nil
}

func (f *FakeBackend) Inspect(_ context.Context, d Desired) (Actual, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := Actual{Warnings: slices.Clone(f.Warnings)}
	if f.Current == nil {
		a.InSync = len(d.Suffixes) == 0 && len(d.CIDRs) == 0
		if !a.InSync {
			a.Diffs = []string{"规则未安装"}
		}
		return a, nil
	}
	a.Installed = true
	a.InSync = slices.Equal(f.Current.Suffixes, d.Suffixes) && slices.Equal(f.Current.CIDRs, d.CIDRs) &&
		slices.Equal(f.Current.Interfaces, d.Interfaces) && f.Current.ListenIP == d.ListenIP && f.Current.ListenPort == d.ListenPort
	if !a.InSync {
		a.Diffs = []string{"规则与配置不一致"}
	}
	return a, nil
}

func (f *FakeBackend) Apply(_ context.Context, d Desired) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailApply != nil {
		return f.FailApply
	}
	f.Current = &d
	f.Applied++
	return nil
}

func (f *FakeBackend) Remove(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Current = nil
	f.Removed++
	return nil
}

func (f *FakeBackend) FlushDNS(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Flushed++
	return nil
}

func (f *FakeBackend) ValidateListen(string, int) error { return nil }

var errNoInterfaces = errors.New("没有找到物理网卡，无法安全地写入防火墙规则")
