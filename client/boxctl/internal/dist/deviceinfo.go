package dist

import (
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
)

// 客户端每次请求都上报设备名和系统版本，管理服务用它们覆盖客户端名称和设备信息。
// 只读一次：设备名、系统版本在服务运行期间基本不变，系统升级后服务也会重启。
var deviceInfo = sync.OnceValues(func() (string, string) {
	name := computerName()
	if name == "" {
		name, _ = os.Hostname()
	}
	return strings.TrimSpace(name), strings.TrimSpace(osVersion())
})

func (c *Client) setInfoHeaders(req *http.Request) {
	name, osv := deviceInfo()
	req.Header.Set(headerAppVersion, c.appVersion)
	req.Header.Set(headerPlatform, runtime.GOOS+"/"+runtime.GOARCH)
	if name != "" {
		req.Header.Set(headerHostname, url.QueryEscape(name))
	}
	if osv != "" {
		req.Header.Set(headerOSVersion, url.QueryEscape(osv))
	}
}
