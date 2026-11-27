package dist

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// OpenBlob 下载管理服务上的文件（更新包、云盘文件）。带 redirect=1 让管理服务回跳转：
// 跳到 OSS 的签名地址时用系统证书校验直连下载（绑定时的证书指纹只认管理服务）；
// 跳回管理服务自己（本机存储的签名地址）时仍用绑定的连接。调用方负责关闭 Body。
func (c *Client) OpenBlob(ctx context.Context, path string) (*http.Response, error) {
	c.mu.Lock()
	b := c.binding
	c.mu.Unlock()
	if b == nil {
		return nil, &InputError{"没有绑定服务端"}
	}
	base, err := url.Parse(b.URL)
	if err != nil {
		return nil, err
	}
	sep := "?"
	if u, _ := url.Parse(path); u != nil && u.RawQuery != "" {
		sep = "&"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL+path+sep+"redirect=1", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.Token)
	c.setInfoHeaders(req)
	hc := httpClient(*b, 0)
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusTemporaryRedirect {
		loc, err := resp.Location()
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("下载地址无效：%w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc.String(), nil)
		if err != nil {
			return nil, err
		}
		next := blobClient
		if loc.Host == base.Host {
			next = httpClient(*b, 0) // 本机存储：签名地址在管理服务上
		}
		if resp, err = next.Do(req); err != nil {
			return nil, err
		}
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, responseError(resp)
	}
	return resp, nil
}

// blobClient 下载 OSS 签名地址：直连（不经系统代理），系统证书校验。只限制建立连接的时间，不限制下载总时长。
var blobClient = &http.Client{Transport: &http.Transport{Proxy: nil, TLSHandshakeTimeout: 15 * time.Second,
	ResponseHeaderTimeout: 60 * time.Second, ForceAttemptHTTP2: true}}

// RemoteFile 是管理服务云盘里的文件（客户端看到的）。
type RemoteFile struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	FileName    string `json:"file_name"`
	Size        int64  `json:"size"`
	Description string `json:"description"`
	Pinned      bool   `json:"pinned"`
	UpdatedAt   string `json:"updated_at"`
	Markdown    bool   `json:"markdown"`
}

// Files 列出管理服务云盘里的文件。
func (c *Client) Files(ctx context.Context) ([]RemoteFile, error) {
	out := []RemoteFile{}
	return out, c.getJSON(ctx, "/api/v1/client/files", &out)
}

// FileContent 读取 Markdown 文件的内容。
func (c *Client) FileContent(ctx context.Context, id int64) (string, error) {
	var out struct {
		Content string `json:"content"`
	}
	err := c.getJSON(ctx, fmt.Sprintf("/api/v1/client/files/%d/content", id), &out)
	return out.Content, err
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	c.mu.Lock()
	b := c.binding
	c.mu.Unlock()
	if b == nil {
		return &InputError{"没有绑定管理服务"}
	}
	resp, err := c.do(ctx, *b, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return responseError(resp)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}
