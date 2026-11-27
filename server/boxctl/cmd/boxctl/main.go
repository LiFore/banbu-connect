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

	"boxctl/internal/buildinfo"
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
	case "reset-password":
		err = runResetPassword(ctx, args)
	case "server":
		err = runServer(ctx, logger.With("role", "server"), args)
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
	fmt.Fprintln(os.Stderr, "用法: boxctl <reset-password|server|version> [参数]")
}
