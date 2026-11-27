package dist

import (
	"os/exec"
	"strings"
)

// computerName 是「系统设置 → 通用 → 关于本机」里的名称（可含中文），比 hostname 更好认。
func computerName() string {
	out, err := exec.Command("/usr/sbin/scutil", "--get", "ComputerName").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func osVersion() string {
	out, err := exec.Command("/usr/bin/sw_vers", "-productVersion").Output()
	if err != nil {
		return "macOS"
	}
	return "macOS " + strings.TrimSpace(string(out))
}
