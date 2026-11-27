package desktop

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// deviceName 是申请接入时默认的设备名称：系统设置里的「电脑名称」，读不到时用主机名。
func deviceName() string {
	if v := command("/usr/sbin/scutil", "--get", "ComputerName"); v != "" {
		return v
	}
	h, _ := os.Hostname()
	return strings.TrimSuffix(h, ".local")
}

// osVersion 返回系统版本，如 macOS 15.2。
func osVersion() string {
	if v := command("/usr/bin/sw_vers", "-productVersion"); v != "" {
		return "macOS " + v
	}
	return "macOS"
}

func command(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
