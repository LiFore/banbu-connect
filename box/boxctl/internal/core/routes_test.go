package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// AutoApply 包了 statusRecorder，WebSocket 升级必须仍能成功（路由盒连接页曾因此一直“正在重连”）。
func TestAutoApplyKeepsWebSocketUpgrade(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer c.CloseNow()
		_ = c.Write(r.Context(), websocket.MessageText, []byte("ok"))
		c.Close(websocket.StatusNormalClosure, "")
	})
	srv := httptest.NewServer(AutoApply(inner, nil, "/api/v1/nodes"))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/box/live", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()
	_, msg, err := c.Read(ctx)
	if err != nil || string(msg) != "ok" {
		t.Fatalf("read = %q, %v", msg, err)
	}
}
