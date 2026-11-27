// boxctl 是 Banbu Connect Lite 的后台程序，由托盘外壳（client/app）拉起。
//
//	boxctl desktop run  本机后台进程：AI 账号管理，不做代理
//	boxctl version      打印版本信息
package main

import (
	"fmt"
	"log/slog"
	"os"

	"boxctl/internal/buildinfo"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "desktop":
		err = runDesktop(args)
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

func usage() {
	fmt.Fprintln(os.Stderr, "用法: boxctl <desktop|version> [参数]")
}
