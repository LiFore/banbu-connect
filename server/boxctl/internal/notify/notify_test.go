package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBark(t *testing.T) {
	var got map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/push" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
	}))
	defer ts.Close()
	b := &Bark{Server: ts.URL + "/", DeviceKeys: []string{" k1 ", ""}, Group: "Banbu"}
	if err := b.Send(context.Background(), Message{Title: "标题", Body: "内容", Urgent: true}); err != nil {
		t.Fatal(err)
	}
	if got["device_key"] != "k1" || got["level"] != "timeSensitive" || got["group"] != "Banbu" || got["title"] != "标题" {
		t.Fatalf("请求内容不对：%v", got)
	}
	b.DeviceKeys = []string{"a", "b"}
	_ = b.Send(context.Background(), Message{Title: "x"})
	if keys, _ := got["device_keys"].([]any); len(keys) != 2 || got["level"] != "active" {
		t.Fatalf("多个设备应一次推送：%v", got)
	}
	if err := (&Bark{Server: ts.URL}).Send(context.Background(), Message{}); err == nil {
		t.Fatal("没有 key 应失败")
	}
}

func TestFeishu(t *testing.T) {
	var got map[string]any
	code := 0
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		if code != 0 {
			_, _ = w.Write([]byte(`{"code":19021,"msg":"sign match fail"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer ts.Close()
	f := &Feishu{Webhook: ts.URL + "/open-apis/bot/v2/hook/x", Secret: "s", Client: ts.Client(), now: func() time.Time { return time.Unix(1599360473, 0) }}
	if err := f.Send(context.Background(), Message{Title: "标题", Body: "内容"}); err != nil {
		t.Fatal(err)
	}
	if got["timestamp"] != "1599360473" || got["sign"] != feishuSign(1599360473, "s") || got["msg_type"] != "text" {
		t.Fatalf("请求内容不对：%v", got)
	}
	if text := got["content"].(map[string]any)["text"].(string); !strings.Contains(text, "标题\n内容") {
		t.Fatalf("文本不对：%q", text)
	}
	code = 1
	if err := f.Send(context.Background(), Message{Title: "x"}); err == nil || !strings.Contains(err.Error(), "19021") {
		t.Fatalf("飞书错误应返回：%v", err)
	}
	if err := (&Feishu{Webhook: "http://insecure"}).Send(context.Background(), Message{}); err == nil {
		t.Fatal("非 https 地址应拒绝")
	}
}
