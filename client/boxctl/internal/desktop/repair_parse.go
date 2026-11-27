package desktop

import (
	"net/url"
	"strings"
)

// parseNetworkServices 解析 networksetup -listallnetworkservices：第一行是说明，带 * 的是已停用的服务。
func parseNetworkServices(out string) []string {
	var list []string
	for i, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if i == 0 || line == "" || strings.HasPrefix(line, "*") {
			continue
		}
		list = append(list, line)
	}
	return list
}

// proxyOnLoopback 解析 networksetup -getwebproxy 等的输出（Enabled / Server，或 URL / Enabled），
// 已启用且指向本机时返回 true。
func proxyOnLoopback(out string) bool {
	enabled, host := false, ""
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "Enabled":
			enabled = v == "Yes"
		case "Server":
			host = v
		case "URL":
			if u, err := url.Parse(v); err == nil {
				host = u.Hostname()
			}
		}
	}
	return enabled && isLoopbackHost(host)
}
