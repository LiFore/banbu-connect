// Package notify 把管理服务的提醒推送到手机：Bark（iOS），以及飞书群机器人（自定义机器人 Webhook）。
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Message 是一条提醒。
type Message struct {
	Title string
	Body  string
	// Urgent 为真时 Bark 用 timeSensitive（专注模式下也会提醒）。
	Urgent bool
	// URL 是点开通知后打开的地址（可选，如管理服务页面）。
	URL string
}

// Sender 是一个推送渠道。
type Sender interface {
	Name() string
	Send(ctx context.Context, m Message) error
}

// DefaultBarkServer 是 Bark 官方服务器；自建服务器时改成自己的地址。
const DefaultBarkServer = "https://api.day.app"

// Bark 推送到 iOS 的 Bark 应用（https://github.com/Finb/Bark）。
type Bark struct {
	Server     string   // 默认 https://api.day.app
	DeviceKeys []string // 一个或多个设备的 key
	Group      string   // 通知分组
	Client     *http.Client
}

func (b *Bark) Name() string { return "Bark" }

func (b *Bark) Send(ctx context.Context, m Message) error {
	server := strings.TrimRight(strings.TrimSpace(b.Server), "/")
	if server == "" {
		server = DefaultBarkServer
	}
	keys := []string{}
	for _, k := range b.DeviceKeys {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return errors.New("没有设备 key")
	}
	level := "active"
	if m.Urgent {
		level = "timeSensitive"
	}
	body := map[string]any{"title": m.Title, "body": m.Body, "group": b.Group, "level": level}
	if m.URL != "" {
		body["url"] = m.URL
	}
	// 多个设备用 device_keys 一次推送（Bark 服务端 v2 接口）。
	if len(keys) == 1 {
		body["device_key"] = keys[0]
	} else {
		body["device_keys"] = keys
	}
	var res struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := postJSON(ctx, b.Client, server+"/push", body, &res); err != nil {
		return err
	}
	if res.Code != 200 {
		return fmt.Errorf("Bark 返回 %d：%s", res.Code, res.Message)
	}
	return nil
}

// Feishu 推送到飞书群的自定义机器人。设置了签名校验时填 Secret。
type Feishu struct {
	Webhook string // https://open.feishu.cn/open-apis/bot/v2/hook/…
	Secret  string
	Client  *http.Client
	now     func() time.Time
}

func (f *Feishu) Name() string { return "飞书" }

// feishuSign 是飞书自定义机器人的签名：以「时间戳\n密钥」为 key 对空串做 HMAC-SHA256，再 Base64。
func feishuSign(ts int64, secret string) string {
	m := hmac.New(sha256.New, []byte(strconv.FormatInt(ts, 10)+"\n"+secret))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func (f *Feishu) Send(ctx context.Context, m Message) error {
	hook := strings.TrimSpace(f.Webhook)
	if u, err := url.Parse(hook); err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("Webhook 地址无效")
	}
	text := m.Title
	if m.Body != "" {
		text += "\n" + m.Body
	}
	if m.URL != "" {
		text += "\n" + m.URL
	}
	body := map[string]any{"msg_type": "text", "content": map[string]string{"text": text}}
	if f.Secret != "" {
		now := time.Now
		if f.now != nil {
			now = f.now
		}
		ts := now().Unix()
		body["timestamp"] = strconv.FormatInt(ts, 10)
		body["sign"] = feishuSign(ts, f.Secret)
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := postJSON(ctx, f.Client, hook, body, &res); err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("飞书返回 %d：%s", res.Code, res.Msg)
	}
	return nil
}

func postJSON(ctx context.Context, c *http.Client, endpoint string, body, out any) error {
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // 不把地址（含 key）写进错误
		}
		return fmt.Errorf("请求失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("HTTP %d，响应无法解析", resp.StatusCode)
	}
	return nil
}
