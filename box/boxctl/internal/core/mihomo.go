package core

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/metacubex/mihomo/config"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/hub"
	"github.com/metacubex/mihomo/hub/executor"
	"github.com/metacubex/mihomo/log"
)

// MihomoVersion 是编进二进制的内核版本：取 go.mod 里的模块版本（C.Version 只有用 mihomo 自己的构建脚本才会写入）。
func MihomoVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if d.Path == "github.com/metacubex/mihomo" {
				if d.Replace != nil && d.Replace.Version != "" {
					return d.Replace.Version
				}
				return d.Version
			}
		}
	}
	return C.Version
}

// RunMihomo 是 `boxctl mihomo` 子命令：把内核编进同一个二进制，
// 所以校验配置和运行配置用的一定是同一个版本。
//
// -watch-stdin：标准输入关闭时退出。父进程无论怎么死，管道都会关闭，
// 这样不会留下孤儿内核占着端口和 TUN。
//
// 路由盒的内核是 procd 的独立服务（boxctl-core，ADR-045）：不带 -watch-stdin，收到 SIGTERM（ctx 结束）
// 时正常退出，撤销 TUN 和 auto-redirect 的 nft 规则；-log-file 把日志写到文件，面板从里面读 TUN 启动失败的原因。
func RunMihomo(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mihomo", flag.ContinueOnError)
	home := fs.String("d", "", "数据目录")
	file := fs.String("f", "", "配置文件")
	test := fs.Bool("t", false, "只校验配置")
	watch := fs.Bool("watch-stdin", false, "标准输入关闭时退出")
	logFile := fs.String("log-file", "", "日志写到这个文件（每次启动清空）；为空时写标准输出")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// 控制接口的 /version 报告编进来的版本，面板据此判断内核是不是需要重启才能更新。
	C.Version = MihomoVersion()
	if *logFile != "" {
		if err := redirectOutput(*logFile); err != nil {
			return fmt.Errorf("打开日志文件失败：%w", err)
		}
	}
	abs, err := filepath.Abs(*home)
	if err != nil {
		return err
	}
	C.SetHomeDir(abs)
	path, err := filepath.Abs(*file)
	if err != nil {
		return err
	}
	C.SetConfig(path)
	if err := config.Init(abs); err != nil {
		return err
	}
	if *test {
		if _, err := executor.Parse(); err != nil {
			return fmt.Errorf("配置校验失败：%w", err)
		}
		return nil
	}
	log.SetLevel(log.WARNING)
	if err := hub.Parse(nil); err != nil {
		return fmt.Errorf("启动内核失败：%w", err)
	}
	defer executor.Shutdown()
	if *watch {
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			stdinClosed <- struct{}{}
		}()
		select {
		case <-stdinClosed:
		case <-ctx.Done():
		}
		return nil
	}
	<-ctx.Done()
	return nil
}

var stdinClosed = make(chan struct{}, 1)
