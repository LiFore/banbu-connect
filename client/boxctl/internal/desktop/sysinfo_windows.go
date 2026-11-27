package desktop

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// deviceName 是申请接入时默认的设备名称：计算机名。
func deviceName() string {
	h, _ := os.Hostname()
	return h
}

// osVersion 返回系统版本，如 Windows 10.0.26100（Windows 11 的主版本号也是 10）。
func osVersion() string {
	v := windows.RtlGetVersion()
	return fmt.Sprintf("Windows %d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
}
