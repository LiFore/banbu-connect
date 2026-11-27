package killswitch

import (
	"context"
	"os"
	"strings"
)

// macOS：单独的 pf 锚点，不写 LaunchDaemon，重启后不会自己加载。
const (
	guardAnchor = "com.apple/boxguard-dns"
	guardConf   = "/var/run/boxguard-dns.conf"
	guardToken  = "/var/run/boxguard-dns.pf-token"
)

type darwinGuard struct{}

func NewSystemGuard() DNSGuard { return darwinGuard{} }

func (darwinGuard) Apply(ctx context.Context, ifaces []string) error {
	if len(ifaces) == 0 {
		return errNoInterfaces
	}
	if err := os.WriteFile(guardConf, []byte(guardPFRules(ifaces)), 0o644); err != nil {
		return err
	}
	// 持有自己的 pf 启用引用：Kill Switch 撤销规则时不会顺带关掉 pf。
	if _, err := os.Stat(guardToken); err != nil {
		out, err := run(ctx, "/sbin/pfctl", "-E")
		if err != nil {
			return err
		}
		for _, line := range strings.Split(out, "\n") {
			if i := strings.Index(line, "Token :"); i >= 0 {
				_ = os.WriteFile(guardToken, []byte(strings.TrimSpace(line[i+len("Token :"):])), 0o600)
			}
		}
	}
	_, err := run(ctx, "/sbin/pfctl", "-a", guardAnchor, "-f", guardConf)
	return err
}

func (darwinGuard) Remove(ctx context.Context) error {
	_, _ = run(ctx, "/sbin/pfctl", "-a", guardAnchor, "-F", "all")
	if t, err := os.ReadFile(guardToken); err == nil {
		_, _ = run(ctx, "/sbin/pfctl", "-X", strings.TrimSpace(string(t)))
		_ = os.Remove(guardToken)
	}
	_ = os.Remove(guardConf)
	return nil
}

func (darwinGuard) Active(ctx context.Context) (bool, error) {
	out, _ := run(ctx, "/sbin/pfctl", "-a", guardAnchor, "-s", "rules")
	return strings.Contains(out, "block"), nil
}
