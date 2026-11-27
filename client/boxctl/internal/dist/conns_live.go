package dist

import (
	"context"
	"net/http"
	"time"
)

// ---- 实时代理连接查看（BXC-F0-030）----
//
// 管理页面打开某台客户端的实时连接时，服务端记下「想看」（1 分钟内有效，页面每几秒刷新一次就一直有效）；
// 客户端每 15 秒问一次（或在每分钟的上报结果里得知），想看的期间每 3 秒推送当前经代理组的连接。
// 推送的连接只放在内存里，不入库。

const maxLiveConns = 300

type liveState struct {
	wantedUntil time.Time
	at          time.Time
	conns       []ConnRecord
}

// LiveView 是管理页面看到的实时连接。At 为空表示客户端还没推送过（可能要等十几秒）。
type LiveView struct {
	At    *time.Time   `json:"at,omitempty"`
	Conns []ConnRecord `json:"conns"`
}

// ---- 客户端 ----

// ConnLiveWanted 问管理服务是否有人在看本机的实时连接。
func (c *Client) ConnLiveWanted(ctx context.Context) (bool, error) {
	var out struct {
		Live bool `json:"live"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/client/conns/live", nil, &out, requestTimeout)
	return out.Live, err
}

// PushConnLive 推送当前经代理组的连接，返回是否还要继续推送。
func (c *Client) PushConnLive(ctx context.Context, conns []ConnRecord) (bool, error) {
	var out struct {
		Live bool `json:"live"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/client/conns/live", map[string]any{"conns": conns}, &out, requestTimeout)
	return out.Live, err
}
