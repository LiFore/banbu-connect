// boxctl 是端侧软路由盒子的管理程序：一个二进制，两个运行角色。
//
//	boxctl panel           Web 管理面板（非关键路径，可随时重启）
//	boxctl guard           proxy-guard 保护守护进程（关键路径，不依赖 panel）
//	boxctl reset-password  重置管理员密码（SSH 上使用）
//	boxctl desktop         桌面客户端（Banbu Connect）的后台服务
//	boxctl server          Banbu Connect 的管理服务：下发配置和更新包
//	boxctl server export-seed  把管理服务当前的配置导出成预置文件
//	boxctl version         打印版本信息
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"boxctl/internal/box"
	"boxctl/internal/buildinfo"
	"boxctl/internal/core"
	"boxctl/internal/guard"
	"boxctl/internal/logring"
	"boxctl/internal/panel"
)

const (
	defaultDataDir  = "/etc/boxctl"
	defaultGuardSck = "/var/run/boxctl/guard.sock"
	defaultPanelSck = "/var/run/boxctl/panel.sock"
	logRingSize     = 2000 // 面板「日志」页和诊断包里能看到的最近条数（M11-03）
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "panel":
		ring := logring.New(logRingSize)
		err = runPanel(ctx, slog.New(logring.NewHandler(logger.Handler(), ring, "panel")).With("role", "panel"), ring, args)
	case "guard":
		ring := logring.New(logRingSize)
		err = runGuard(ctx, slog.New(logring.NewHandler(logger.Handler(), ring, "guard")).With("role", "guard"), ring, args)
	case "reset-password":
		err = runResetPassword(ctx, args)
	case "mihomo":
		err = runMihomo(ctx, args)
	case "status":
		err = runStatus(ctx, args)
	case "version":
		fmt.Println(buildinfo.String())
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		logger.Error("exit", "err", err)
		os.Exit(1)
	}
}

func runPanel(ctx context.Context, logger *slog.Logger, ring *logring.Ring, args []string) error {
	fs := flag.NewFlagSet("panel", flag.ExitOnError)
	opts := panel.Options{Logger: logger}
	fs.StringVar(&opts.Listen, "listen", "127.0.0.1:8443", "HTTPS 监听地址")
	fs.StringVar(&opts.HTTPListen, "http-listen", "", "HTTP 跳转 HTTPS 的监听地址，空则不监听")
	fs.StringVar(&opts.DataDir, "data-dir", defaultDataDir, "数据目录（数据库、设备密钥、证书）")
	fs.StringVar(&opts.GuardSocket, "guard-sock", defaultGuardSck, "guard 的 unix socket 路径")
	fs.StringVar(&opts.FetchProxy, "fetch-proxy", "http://127.0.0.1:7890", "订阅经代理拉取时使用的本地代理（Mihomo mixed-port）")
	exclude := fs.String("route-exclude", "", "TUN 不接管的目标网段，逗号分隔（局域网网段）")
	rfc6761 := fs.Bool("dnsmasq-rfc6761", false, "init 脚本关掉了 dnsmasq 的 boguspriv，由 guard 补回（.local 除外）")
	lanDev := fs.String("lan-dev", "br-lan", "局域网网桥：读邻居表发现设备")
	statusSock := fs.String("status-sock", defaultPanelSck, "本机状态接口的 unix socket 路径（boxctl status、LuCI 用），空则不提供")
	coreInit := fs.String("core-init", "/etc/init.d/boxctl-core", "内核独立服务的 init 脚本；不存在时内核作为面板的子进程运行")
	corePID := fs.String("core-pidfile", "/var/run/boxctl/core.pid", "内核服务的 pid 文件（procd 写）")
	coreLog := fs.String("core-log", "/var/run/boxctl/core.log", "内核服务的日志文件")
	discovery := box.DiscoveryOptions{Secret: os.Getenv("BOXCTL_DISCOVERY_SECRET")} // 口令经环境变量传，不出现在进程列表里
	fs.StringVar(&discovery.Listen, "discovery-listen", "", "客户端发现路由盒的应答地址（LAN 地址:19443），空则不开")
	fs.StringVar(&discovery.ID, "discovery-id", "", "路由盒 ID，客户端按它记住出口；空则用主机名")
	_ = fs.Parse(args)
	// 路由盒：面板里托管内核、严格线路检查和管理服务绑定，界面与桌面客户端同款。
	b := box.New(ctx, box.Options{DataDir: opts.DataDir, GuardSocket: opts.GuardSocket, StatusSocket: *statusSock,
		CoreService: core.ServiceOptions{Init: *coreInit, PIDFile: *corePID, LogFile: *coreLog}, RouteExclude: splitList(*exclude), DnsmasqRFC6761: *rfc6761, Logger: logger, Logs: ring, LANDevice: *lanDev,
		Discovery: discovery})
	opts.Box, opts.Extend, opts.Wrap = true, b.Extend, b.Wrap
	return panel.Run(ctx, opts)
}

func runGuard(ctx context.Context, logger *slog.Logger, ring *logring.Ring, args []string) error {
	fs := flag.NewFlagSet("guard", flag.ExitOnError)
	opts := guard.Options{Logger: logger, Logs: ring}
	fs.StringVar(&opts.Socket, "sock", defaultGuardSck, "unix socket 路径")
	fs.StringVar(&opts.DataDir, "data-dir", defaultDataDir, "数据目录（保存上一份策略）")
	fs.StringVar(&opts.LANDev, "lan-dev", "br-lan", "局域网网桥：局域网的 DNS 重定向到本机、hold 时阻断")
	fs.BoolVar(&opts.Fake, "fake", false, "不改动系统（开发用）；非 Linux 上总是如此")
	uninstall := fs.Bool("uninstall", false, "删除保护规则和 dnsmasq 片段、恢复改过的系统设置后退出（卸载包时用）")
	_ = fs.Parse(args)
	if *uninstall {
		return guard.Uninstall(ctx, opts.DataDir)
	}
	return guard.Run(ctx, opts)
}

func runResetPassword(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("reset-password", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir, "数据目录")
	_ = fs.Parse(args)

	db, authSvc, err := panel.OpenAuth(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := authSvc.ResetPassword(ctx); err != nil {
		return err
	}
	fmt.Printf("已重置，所有会话已失效。新的初始密码在 %s（首次登录后须修改）\n", authSvc.InitialPasswordPath)
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func usage() {
	fmt.Fprintln(os.Stderr, "用法: boxctl <panel|guard|status|reset-password|mihomo|version> [参数]")
}
