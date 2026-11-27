//go:build !darwin && !windows

package desktop

import (
	"os"
	"path/filepath"
)

// 其他平台只用于开发和测试：客户端只发布 macOS 和 Windows 版。

func dataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, ".local", "share", dirName)
}

func deviceName() string {
	h, _ := os.Hostname()
	return h
}

func osVersion() string { return "unknown" }
