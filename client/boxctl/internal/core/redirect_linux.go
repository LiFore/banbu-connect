//go:build linux

package core

import (
	"os"

	"golang.org/x/sys/unix"
)

// redirectOutput 把标准输出和标准错误换成日志文件（内核和 Go 运行时的输出都进去）。
func redirectOutput(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := unix.Dup3(int(f.Fd()), 1, 0); err != nil {
		return err
	}
	return unix.Dup3(int(f.Fd()), 2, 0)
}
