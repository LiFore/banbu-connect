package sub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultUserAgent = "clash.meta" // 多数机场据此返回 Mihomo 兼容的 YAML
	maxBodyBytes     = 20 << 20
)

type FetchRequest struct {
	URL       string
	UserAgent string
	ProxyURL  string // 空表示直连（M2-02）
	Timeout   time.Duration
	Retries   int
}

type FetchResult struct {
	Body     []byte
	UserInfo *UserInfo // 响应头缺失时为 nil
}

// Fetch 拉取订阅。返回的错误不含 URL：订阅链接里通常带 token，不能进日志和 last_error。
func Fetch(ctx context.Context, req FetchRequest) (FetchResult, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // 不读取环境变量代理，行为只由 ProxyURL 决定
	if req.ProxyURL != "" {
		pu, err := url.Parse(req.ProxyURL)
		if err != nil {
			return FetchResult{}, fmt.Errorf("代理地址无效：%v", err)
		}
		transport.Proxy = http.ProxyURL(pu)
	}
	client := &http.Client{Transport: transport, Timeout: req.Timeout}
	defer transport.CloseIdleConnections()

	var lastErr error
	for attempt := 0; attempt <= req.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return FetchResult{}, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		res, err := fetchOnce(ctx, client, req)
		if err == nil {
			return res, nil
		}
		lastErr = err
		var se statusError
		if errors.As(err, &se) && se.code >= 400 && se.code < 500 && se.code != http.StatusTooManyRequests {
			break // 4xx（除 429）重试无意义
		}
	}
	return FetchResult{}, lastErr
}

type statusError struct{ code int }

func (e statusError) Error() string { return fmt.Sprintf("订阅服务器返回 HTTP %d", e.code) }

func fetchOnce(ctx context.Context, client *http.Client, req FetchRequest) (FetchResult, error) {
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return FetchResult{}, errors.New("订阅地址无效")
	}
	ua := req.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	hr.Header.Set("User-Agent", ua)

	resp, err := client.Do(hr)
	if err != nil {
		return FetchResult{}, stripURL(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return FetchResult{}, statusError{resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return FetchResult{}, stripURL(err)
	}
	if len(body) > maxBodyBytes {
		return FetchResult{}, fmt.Errorf("订阅内容超过 %d MB", maxBodyBytes>>20)
	}
	return FetchResult{Body: body, UserInfo: ParseUserInfo(resp.Header.Get("Subscription-Userinfo"))}, nil
}

// stripURL 去掉 *url.Error 中的完整 URL。
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return errors.New("请求超时")
		}
		return fmt.Errorf("请求失败：%v", ue.Err)
	}
	return err
}

// UserInfo 对应响应头 subscription-userinfo（M2-04），单位字节；Expire 为 Unix 秒，0 表示未知。
type UserInfo struct {
	Upload   int64 `json:"upload"`
	Download int64 `json:"download"`
	Total    int64 `json:"total"`
	Expire   int64 `json:"expire"`
}

// ParseUserInfo 解析 "upload=1; download=2; total=3; expire=4"。无任何有效字段时返回 nil。
func ParseUserInfo(h string) *UserInfo {
	var ui UserInfo
	found := false
	for _, part := range strings.Split(h, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64) // 部分机场输出科学计数法
		if err != nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "upload":
			ui.Upload = int64(n)
		case "download":
			ui.Download = int64(n)
		case "total":
			ui.Total = int64(n)
		case "expire":
			ui.Expire = int64(n)
		default:
			continue
		}
		found = true
	}
	if !found {
		return nil
	}
	return &ui
}
