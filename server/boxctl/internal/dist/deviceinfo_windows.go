package dist

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// computerName 用 os.Hostname（Windows 的计算机名）。
func computerName() string { return "" }

// osVersion 形如 Windows 11 Pro 24H2（26100）。Windows 11 的 ProductName 仍写着 Windows 10，按版本号改正。
func osVersion() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return "Windows"
	}
	defer k.Close()
	product, _, _ := k.GetStringValue("ProductName")
	display, _, _ := k.GetStringValue("DisplayVersion")
	build, _, _ := k.GetStringValue("CurrentBuildNumber")
	var n int
	if _, err := fmt.Sscan(build, &n); err == nil && n >= 22000 {
		product = strings.Replace(product, "Windows 10", "Windows 11", 1)
	}
	if product == "" {
		product = "Windows"
	}
	s := strings.TrimSpace(product + " " + display)
	if build != "" {
		s += "（" + build + "）"
	}
	return s
}
