package guard

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// System 把规则落到系统上。规则写进内核的 nft 表，不依赖 guard 进程存活。
type System interface {
	// Apply 执行一份 nft 脚本（一个事务）。
	Apply(ctx context.Context, script string) error
	// Installed 报告系统上的规则是不是 version 这一份。
	Installed(ctx context.Context, version string) (bool, error)
	// WriteDnsmasq 写 dnsmasq 片段，内容变化时重启 dnsmasq。
	WriteDnsmasq(ctx context.Context, conf string) error
	// Remove 删除 guard 的表和 dnsmasq 片段。
	Remove(ctx context.Context) error
	// Dump 返回系统上 guard 的表和 dnsmasq 片段，诊断用。
	Dump(ctx context.Context) string
}

// execSystem 调用 nft 和 dnsmasq 的 init 脚本。
type execSystem struct{}

func NewSystem() System { return execSystem{} }

func run(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s：%w：%s", name, err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

func (execSystem) Apply(ctx context.Context, script string) error {
	_, err := run(ctx, script, "nft", "-f", "-")
	return err
}

func init() {
	// nft 按进程的时区解释 meta hour（设备时段管控）。procd 启动的进程不一定带 TZ，
	// OpenWrt 把时区写在 /etc/TZ。TODO(verify:R5) iStoreOS 实机确认时段按本地时间生效。
	if os.Getenv("TZ") == "" {
		if b, err := os.ReadFile("/etc/TZ"); err == nil {
			if tz := strings.TrimSpace(string(b)); tz != "" {
				_ = os.Setenv("TZ", tz)
			}
		}
	}
}

func (execSystem) Installed(ctx context.Context, version string) (bool, error) {
	out, err := run(ctx, "", "nft", "list", "chain", "inet", Table, "output")
	if err != nil {
		// 表或链不存在：nft 返回非零。其他错误也按未安装处理，下一步重装会报出真正的原因。
		return false, nil
	}
	return strings.Contains(out, versionTag+version), nil
}

const dnsmasqFile = "boxctl.conf"

// dnsmasqDir 找 dnsmasq 读取的配置片段目录。OpenWrt 23.05 起每个实例有自己的目录。
// TODO(verify:R3) iStoreOS 24.10 实机确认目录。
func dnsmasqDir(ctx context.Context) string {
	if out, err := run(ctx, "", "uci", "-q", "get", "dhcp.@dnsmasq[0].confdir"); err == nil {
		if d := strings.TrimSpace(out); d != "" {
			return d
		}
	}
	if m, _ := filepath.Glob("/tmp/dnsmasq.*.d"); len(m) > 0 {
		return m[0]
	}
	return "/tmp/dnsmasq.d"
}

func (execSystem) WriteDnsmasq(ctx context.Context, conf string) error {
	dir := dnsmasqDir(ctx)
	path := filepath.Join(dir, dnsmasqFile)
	if old, err := os.ReadFile(path); err == nil && string(old) == conf {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(conf), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// conf-dir 里的变化要重启才生效；SIGHUP 只重读 hosts。
	_, err := run(ctx, "", "/etc/init.d/dnsmasq", "restart")
	return err
}

func (execSystem) Remove(ctx context.Context) error {
	_ = os.Remove(filepath.Join(dnsmasqDir(ctx), dnsmasqFile))
	_, _ = run(ctx, "", "/etc/init.d/dnsmasq", "restart")
	if _, err := run(ctx, "", "nft", "list", "table", "inet", Table); err != nil {
		return nil // 表不在，不用删
	}
	_, err := run(ctx, RemoveScript(), "nft", "-f", "-")
	return err
}

func (execSystem) Dump(ctx context.Context) string {
	var b strings.Builder
	out, err := run(ctx, "", "nft", "list", "table", "inet", Table)
	if err != nil {
		out = err.Error()
	}
	b.WriteString("# nft list table inet " + Table + "\n" + out + "\n")
	conf, err := os.ReadFile(filepath.Join(dnsmasqDir(ctx), dnsmasqFile))
	if err != nil {
		conf = []byte(err.Error())
	}
	b.WriteString("# " + dnsmasqFile + "\n" + string(conf))
	return b.String()
}

// FakeSystem 在内存里记录规则，用于开发和测试。
type FakeSystem struct {
	mu      sync.Mutex
	Script  string
	Dnsmasq string
	Applies int
	Fail    error
	Gone    bool // 模拟 fw4 restart 或手工删表
}

func (f *FakeSystem) Apply(_ context.Context, script string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Fail != nil {
		return f.Fail
	}
	f.Script, f.Gone = script, false
	f.Applies++
	return nil
}

func (f *FakeSystem) Installed(_ context.Context, version string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.Gone && f.Script != "" && strings.Contains(f.Script, versionTag+version), nil
}

func (f *FakeSystem) WriteDnsmasq(_ context.Context, conf string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Dnsmasq = conf
	return nil
}

func (f *FakeSystem) Remove(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Script, f.Dnsmasq = "", ""
	return nil
}

func (f *FakeSystem) Dump(context.Context) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return "# fake\n" + f.Script + "\n# " + dnsmasqFile + "\n" + f.Dnsmasq
}

// Snapshot 返回当前记录的规则，测试中读取时加锁。
func (f *FakeSystem) Snapshot() (script, dnsmasq string, applies int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Script, f.Dnsmasq, f.Applies
}

// Drop 模拟表被删除（fw4 restart、手工 nft delete table）。
func (f *FakeSystem) Drop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Gone = true
}
