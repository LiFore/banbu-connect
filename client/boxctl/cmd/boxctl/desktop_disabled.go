//go:build !windows && !darwin

package main

import "errors"

// 路由盒不需要桌面服务。
func runDesktop([]string) error { return errors.New("只有桌面平台支持这个命令") }
