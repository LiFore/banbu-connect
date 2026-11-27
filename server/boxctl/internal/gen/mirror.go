package gen

import (
	"fmt"
	"net/url"
	"strings"
)

// 下载镜像（M7-04）：内置的规则集、geox 数据都从 jsDelivr 下载（GitHub 仓库的 CDN，不是第三方加速域名）。
// 国内直连 jsDelivr 不稳定时，可以设置一个与 jsDelivr 路径兼容的镜像（如自建反代、其他 jsDelivr 节点），
// 凡是 jsDelivr 的地址都改写到镜像上；其他地址（管理服务下发的自定义规则集）不变。

var jsdelivrHosts = map[string]bool{
	"cdn.jsdelivr.net":          true,
	"testingcf.jsdelivr.net":    true,
	"fastly.jsdelivr.net":       true,
	"gcore.jsdelivr.net":        true,
	"originfastly.jsdelivr.net": true,
}

// ValidateMirror 检查镜像地址：https，带主机名，不带查询参数。空串表示不用镜像。
func ValidateMirror(m string) (string, error) {
	m = strings.TrimSpace(m)
	if m == "" {
		return "", nil
	}
	u, err := url.Parse(m)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("镜像地址须是 https://主机[/路径前缀]，不带参数")
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

// MirrorURL 把 jsDelivr 的地址改写到镜像：https://cdn.jsdelivr.net/gh/x → <mirror>/gh/x。
func MirrorURL(raw, mirror string) string {
	if mirror == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || !jsdelivrHosts[strings.ToLower(u.Host)] {
		return raw
	}
	return mirror + u.EscapedPath()
}

// mirrorHost 是镜像的主机名，用于让内核下载 geo 数据的请求固定直连。
func mirrorHost(mirror string) string {
	if u, err := url.Parse(mirror); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return geoHost
}
