package main

import (
	"context"

	"boxctl/internal/core"
)

// Mihomo 内核编进同一个二进制：桌面客户端和路由盒都由 boxctl 自己运行内核。
// ctx 在 SIGTERM、SIGINT 时结束，内核正常退出（撤销 TUN 和路由）。
func runMihomo(ctx context.Context, args []string) error { return core.RunMihomo(ctx, args) }
