package box

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// confirmURLs 是应用后健康确认的探测地址：经内核的混合端口按规则走，
// 境外地址会走代理组，出口不通（节点全坏、规则把流量导到不存在的出口）时探测失败（M8-06）。
var confirmURLs = []string{
	"https://www.gstatic.com/generate_204",
	"https://cp.cloudflare.com/generate_204",
}

// confirmEgress 确认新配置下内核在运行、TUN 已接管，且经内核能访问境外。任一地址成功即可。
func (b *Box) confirmEgress(ctx context.Context) error {
	if !b.sup.Status().Running {
		return errors.New("内核没有运行")
	}
	code, body, err := b.sup.CallCore(ctx, http.MethodGet, "/configs", nil)
	if err != nil {
		return fmt.Errorf("内核控制接口无响应：%w", err)
	}
	var cfg struct {
		MixedPort int `json:"mixed-port"`
		TUN       struct {
			Enable bool `json:"enable"`
		} `json:"tun"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &cfg) != nil {
		return fmt.Errorf("读取内核配置失败（HTTP %d）", code)
	}
	if !cfg.TUN.Enable {
		return errors.New("TUN 没有启动，局域网流量没有被接管")
	}
	if cfg.MixedPort == 0 {
		return errors.New("内核没有混合端口，无法探测")
	}
	proxy := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(cfg.MixedPort)}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true}}
	var errs []string
	for _, u := range confirmURLs {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode < 400 {
			return nil
		}
		errs = append(errs, fmt.Sprintf("%s 返回 HTTP %d", u, resp.StatusCode))
	}
	return fmt.Errorf("经内核访问境外失败：%s", strings.Join(errs, "；"))
}
