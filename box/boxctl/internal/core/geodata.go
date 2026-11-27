package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"boxctl/internal/gen"
)

// 内核校验和启动时要读 geo 数据（geodata-mode：GeoIP.dat、GeoSite.dat），缺了会自己下载。
// 它只认一个地址，国内直连 jsDelivr 经常失败，配置就一直校验不过。所以在交给内核之前，
// 先由服务把文件准备好：随应用打包的文件 → 多个镜像直连 → 经本机代理。

type geoFile struct {
	name  string // 内核目录里的文件名
	asset string // meta-rules-dat 里的文件名
}

var geoFiles = []geoFile{
	{"GeoIP.dat", "geoip-lite.dat"},
	{"GeoSite.dat", "geosite.dat"},
}

// geoMirrors 依次尝试。最后一个是 GitHub 原始地址，经代理时最可靠。
var geoMirrors = []string{
	"https://testingcf.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@release/",
	"https://fastly.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@release/",
	"https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@release/",
	"https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/",
}

const (
	geoMinSize       = 64 << 10 // 小于这个大小的一定是错误页
	geoDownloadLimit = 64 << 20
)

// SeedGeoData 把 src 目录里随应用打包的 geo 文件复制到内核目录，已有的不覆盖。
func SeedGeoData(src, coreDir string) error {
	if err := os.MkdirAll(coreDir, 0o700); err != nil {
		return err
	}
	for _, f := range geoFiles {
		dst := filepath.Join(coreDir, f.name)
		if geoPresent(dst) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, f.name))
		if err != nil || len(b) < geoMinSize {
			continue
		}
		if err := writeFileAtomic(dst, b); err != nil {
			return err
		}
	}
	return nil
}

// ensureGeoData 保证内核目录里有 geo 文件。proxy 是本机代理地址，内核没运行时为空；
// mirror 是用户设置的下载镜像（M7-04），设置了就最先尝试。
func ensureGeoData(ctx context.Context, bundled, coreDir, proxy, mirror string) error {
	if err := SeedGeoData(bundled, coreDir); err != nil {
		return err
	}
	var errs []string
	for _, f := range geoFiles {
		dst := filepath.Join(coreDir, f.name)
		if geoPresent(dst) {
			continue
		}
		if err := downloadGeo(ctx, f, dst, proxy, mirror); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("下载 geo 数据失败（%s）。打开代理后会经代理重试", strings.Join(errs, "；"))
	}
	return nil
}

func geoPresent(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Size() >= geoMinSize
}

func downloadGeo(ctx context.Context, f geoFile, dst, proxy, mirror string) error {
	bases := geoMirrors
	if mirror != "" {
		bases = append([]string{gen.MirrorURL(geoMirrors[0], mirror) + "/"}, geoMirrors...)
	}
	type route struct {
		label string
		proxy *url.URL
	}
	routes := []route{{"直连", nil}}
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			routes = append(routes, route{"经代理", u})
		}
	}
	var last error
	for _, r := range routes {
		client := &http.Client{
			Timeout:   2 * time.Minute,
			Transport: &http.Transport{Proxy: http.ProxyURL(r.proxy), TLSHandshakeTimeout: 10 * time.Second},
		}
		for _, base := range bases {
			b, err := fetchGeo(ctx, client, base+f.asset)
			if err == nil {
				return writeFileAtomic(dst, b)
			}
			last = fmt.Errorf("%s %s：%w", r.label, hostOnly(base), err)
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	return fmt.Errorf("%s：%w", f.name, last)
}

func fetchGeo(ctx context.Context, client *http.Client, u string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, geoDownloadLimit))
	if err != nil {
		return nil, err
	}
	if len(b) < geoMinSize {
		return nil, errors.New("文件不完整")
	}
	return b, nil
}

func hostOnly(u string) string {
	if p, err := url.Parse(u); err == nil {
		return p.Host
	}
	return u
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
