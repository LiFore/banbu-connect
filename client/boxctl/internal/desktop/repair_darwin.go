package desktop

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const networksetup = "/usr/sbin/networksetup"

// clearLoopbackProxy 关闭所有网络服务上指向本机的 HTTP、HTTPS、SOCKS 代理和自动代理（PAC）。
func clearLoopbackProxy(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, networksetup, "-listallnetworkservices").Output()
	if err != nil {
		return fmt.Errorf("列出网络服务：%w", err)
	}
	var errs []error
	for _, svc := range parseNetworkServices(string(out)) {
		for _, kind := range [][2]string{
			{"-getwebproxy", "-setwebproxystate"},
			{"-getsecurewebproxy", "-setsecurewebproxystate"},
			{"-getsocksfirewallproxy", "-setsocksfirewallproxystate"},
			{"-getautoproxyurl", "-setautoproxystate"},
		} {
			info, err := exec.CommandContext(ctx, networksetup, kind[0], svc).Output()
			if err != nil || !proxyOnLoopback(string(info)) {
				continue
			}
			if out, err := exec.CommandContext(ctx, networksetup, kind[1], svc, "off").CombinedOutput(); err != nil {
				errs = append(errs, fmt.Errorf("%s %s：%w：%s", svc, kind[1], err, strings.TrimSpace(string(out))))
			}
		}
	}
	return errors.Join(errs...)
}
