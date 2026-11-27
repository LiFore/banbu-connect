package dist

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"

	"boxctl/internal/otel"
)

// Claude Code 遥测：客户端把本机收到的 OTel 数据（不含提示词和助手回复原文）分批上报，
// 管理员在「客户端」页面看每台客户端的 Claude Code 用量。数据存在管理服务数据目录的 otel.db。

const (
	maxOTelUpload = 32 << 20 // 解压后的大小上限
	otelKeepDays  = 90       // 服务端保留天数
)

// ---- 客户端 ----

// ReportOTel 上报一批遥测数据（gzip 压缩）。没有绑定时什么也不做，返回 false。
func (c *Client) ReportOTel(ctx context.Context, b otel.Batch) (bool, error) {
	c.mu.Lock()
	bind := c.binding
	c.mu.Unlock()
	if bind == nil {
		return false, nil
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(b); err != nil {
		return true, err
	}
	if err := zw.Close(); err != nil {
		return true, err
	}
	resp, err := c.doMethod(ctx, *bind, http.MethodPost, "/api/v1/client/otel", &buf, func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Encoding", "gzip")
	})
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return true, responseError(resp)
	}
	return true, nil
}

// ---- 服务端 ----
