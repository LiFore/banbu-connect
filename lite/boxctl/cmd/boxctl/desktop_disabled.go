//go:build !windows && !darwin

package main

import "errors"

// 只支持 macOS 和 Windows。
func runDesktop([]string) error { return errors.New("只有桌面平台支持这个命令") }
