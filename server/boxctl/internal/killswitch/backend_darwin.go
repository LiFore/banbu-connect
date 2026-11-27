package killswitch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// macOS：/etc/resolver 负责 DNS 层，pf 负责 IP 层。
//
// TODO(verify:V1) 规则已能加载，但还没有证明 pf 真的拦截了流量。
// TODO(verify:V3) 开机由 LaunchDaemon 加载 pf，重启后的效果还没有确认。

const (
	pfAnchor    = "com.apple/boxguard"
	confDir     = "/etc/boxguard"
	resolverDir = "/etc/resolver"
	pfPlist     = "/Library/LaunchDaemons/com.boxguard.pf.plist"
	pfLabel     = "com.boxguard.pf"
	pfToken     = "/var/run/boxguard.pf-token"
)

// 先启用新引用再释放旧的，pf 不会有关闭的间隙。
const loadPFScript = `#!/bin/sh
OLD=$(cat "` + pfToken + `" 2>/dev/null)
/sbin/pfctl -E 2>&1 | /usr/bin/awk '/Token/ {print $NF}' > "` + pfToken + `"
[ -n "$OLD" ] && /sbin/pfctl -X "$OLD" >/dev/null 2>&1
/sbin/pfctl -a "` + pfAnchor + `" -f "` + confDir + `/pf.conf"
`

const pfPlistContent = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + pfLabel + `</string>
  <key>ProgramArguments</key>
  <array><string>/bin/sh</string><string>` + confDir + `/load-pf.sh</string></array>
  <key>RunAtLoad</key><true/>
</dict>
</plist>
`

type darwinBackend struct{}

func NewSystemBackend() Backend { return darwinBackend{} }

func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w: %s", filepath.Base(name), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (darwinBackend) Interfaces(ctx context.Context) ([]string, error) {
	out, err := run(ctx, "/usr/sbin/networksetup", "-listallhardwareports")
	if err != nil {
		return nil, err
	}
	return parseHardwarePorts(out), nil
}

// taggedResolvers 只列出本工具写的文件。
func taggedResolvers() (map[string]string, error) {
	out := map[string]string{}
	entries, err := os.ReadDir(resolverDir)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(resolverDir, e.Name()))
		if err == nil && strings.HasPrefix(string(b), resolverTag+"\n") {
			out[e.Name()] = string(b)
		}
	}
	return out, nil
}

func (darwinBackend) Inspect(ctx context.Context, d Desired) (Actual, error) {
	files, err := taggedResolvers()
	if err != nil {
		return Actual{}, err
	}
	rules, _ := run(ctx, "/sbin/pfctl", "-a", pfAnchor, "-s", "rules")
	rulesLoaded := strings.Contains(rules, "block")
	_, plistErr := os.Stat(pfPlist)

	a := Actual{Installed: len(files) > 0 || rulesLoaded || plistErr == nil}
	want := resolverContent(d.ListenIP, d.ListenPort)
	for _, s := range d.Suffixes {
		if files[s] != want {
			a.Diffs = append(a.Diffs, "DNS 路由缺失或有变化："+s)
		}
	}
	wanted := map[string]bool{}
	for _, s := range d.Suffixes {
		wanted[s] = true
	}
	for name := range files {
		if !wanted[name] {
			a.Diffs = append(a.Diffs, "多余的 DNS 路由："+name)
		}
	}
	if len(d.CIDRs) > 0 {
		conf, _ := os.ReadFile(filepath.Join(confDir, "pf.conf"))
		if string(conf) != pfRules(d) {
			a.Diffs = append(a.Diffs, "pf 规则的网段或网卡有变化")
		}
		if !rulesLoaded {
			a.Diffs = append(a.Diffs, "pf 规则没有加载")
		}
		info, _ := run(ctx, "/sbin/pfctl", "-s", "info")
		if !strings.Contains(info, "Status: Enabled") {
			a.Diffs = append(a.Diffs, "pf 没有启用")
		}
	} else if rulesLoaded {
		a.Diffs = append(a.Diffs, "存在多余的 pf 规则")
	}
	a.InSync = len(a.Diffs) == 0
	return a, nil
}

func (b darwinBackend) Apply(ctx context.Context, d Desired) error {
	// 先检查再改动：不覆盖别的程序或用户写的 resolver 文件。
	tagged, err := taggedResolvers()
	if err != nil {
		return err
	}
	for _, s := range d.Suffixes {
		if _, ours := tagged[s]; ours {
			continue
		}
		if _, err := os.Stat(filepath.Join(resolverDir, s)); err == nil {
			return fmt.Errorf("%s/%s 已存在且不是本工具创建的", resolverDir, s)
		}
	}
	if err := os.MkdirAll(resolverDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return err
	}
	want := resolverContent(d.ListenIP, d.ListenPort)
	wanted := map[string]bool{}
	for _, s := range d.Suffixes {
		wanted[s] = true
		if err := writeFileAtomic(filepath.Join(resolverDir, s), []byte(want), 0o644); err != nil {
			return err
		}
	}
	for name := range tagged {
		if !wanted[name] {
			if err := os.Remove(filepath.Join(resolverDir, name)); err != nil {
				return err
			}
		}
	}
	if err := writeFileAtomic(filepath.Join(confDir, "pf.conf"), []byte(pfRules(d)), 0o644); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(confDir, "load-pf.sh"), []byte(loadPFScript), 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(pfPlist, []byte(pfPlistContent), 0o644); err != nil {
		return err
	}
	if _, err := run(ctx, "/bin/launchctl", "print", "system/"+pfLabel); err != nil {
		if _, err := run(ctx, "/bin/launchctl", "bootstrap", "system", pfPlist); err != nil {
			return err
		}
	}
	if _, err := run(ctx, "/bin/sh", filepath.Join(confDir, "load-pf.sh")); err != nil {
		return err
	}
	return b.FlushDNS(ctx)
}

func (b darwinBackend) Remove(ctx context.Context) error {
	_, _ = run(ctx, "/bin/launchctl", "bootout", "system", pfPlist)
	_ = os.Remove(pfPlist)
	_, _ = run(ctx, "/sbin/pfctl", "-a", pfAnchor, "-F", "all")
	if t, err := os.ReadFile(pfToken); err == nil {
		_, _ = run(ctx, "/sbin/pfctl", "-X", strings.TrimSpace(string(t)))
		_ = os.Remove(pfToken)
	}
	_ = os.RemoveAll(confDir)
	tagged, err := taggedResolvers()
	if err != nil {
		return err
	}
	for name := range tagged {
		if err := os.Remove(filepath.Join(resolverDir, name)); err != nil {
			return err
		}
	}
	return b.FlushDNS(ctx)
}

func (darwinBackend) FlushDNS(ctx context.Context) error {
	if _, err := run(ctx, "/usr/bin/dscacheutil", "-flushcache"); err != nil {
		return err
	}
	_, _ = run(ctx, "/usr/bin/killall", "-HUP", "mDNSResponder")
	return nil
}

func (darwinBackend) ValidateListen(ip string, _ int) error {
	if ip != "127.0.0.1" {
		return &ValidationError{"macOS 上 DNS 入口须为 127.0.0.1（其他回环地址默认不可用）"}
	}
	return nil
}
