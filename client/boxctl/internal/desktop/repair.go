package desktop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"boxctl/internal/killswitch"
)

// repairSystem 是外壳「修复工具」调用的 `boxctl desktop repair`：后台服务起不来时，
// 撤销留在系统里的规则，让受保护的服务和 DNS 恢复直连。需要管理员权限，不碰后台服务本身和数据。
//   - Kill Switch：NRPT / /etc/resolver（DNS 层）和受保护网段的防火墙 / pf 规则（IP 层）
//   - DNS 防泄漏：物理网卡上拦截 53、853 端口的防火墙 / pf 规则
//   - 系统代理：指向本机（127.0.0.1、localhost）的代理设置。Windows 的系统代理是用户设置，由外壳在提权前清除。
//
// 后台服务恢复运行后，Kill Switch 和 DNS 防泄漏如果仍是开启的，会重新安装这些规则。
func repairSystem() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var errs []error
	if err := killswitch.NewSystemBackend().Remove(ctx); err != nil {
		errs = append(errs, fmt.Errorf("撤销保护服务规则失败：%w", err))
	}
	if err := killswitch.NewSystemGuard().Remove(ctx); err != nil {
		errs = append(errs, fmt.Errorf("撤销 DNS 防泄漏规则失败：%w", err))
	}
	if err := clearLoopbackProxy(ctx); err != nil {
		errs = append(errs, fmt.Errorf("关闭系统代理失败：%w", err))
	}
	return errors.Join(errs...)
}

// isLoopbackHost 判断代理地址是否指向本机。
func isLoopbackHost(h string) bool {
	h = strings.ToLower(strings.Trim(strings.TrimSpace(h), "[]"))
	return h == "localhost" || h == "::1" || strings.HasPrefix(h, "127.")
}
