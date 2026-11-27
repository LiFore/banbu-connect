package dist

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"boxctl/internal/notify"
)

// 消息推送（S1.12）：Bark 和飞书群机器人。设备 key、Webhook、签名密钥加密保存。

const (
	notifyKey = "notify"
	notifyAAD = "notify"
	maxLog    = 200
)

// notifySetting 是保存的配置；敏感项放在加密的 sealed 里。
type notifySetting struct {
	BarkEnabled   bool   `json:"bark_enabled"`
	BarkServer    string `json:"bark_server"`
	BarkGroup     string `json:"bark_group"`
	FeishuEnabled bool   `json:"feishu_enabled"`
	PanelURL      string `json:"panel_url"` // 点开通知后打开的管理服务地址（可选）
	Sealed        string `json:"sealed,omitempty"`
}

type notifySecrets struct {
	BarkKeys     []string `json:"bark_keys"`
	FeishuHook   string   `json:"feishu_hook"`
	FeishuSecret string   `json:"feishu_secret"`
}

// NotifyView 是给管理页面看的配置：敏感项只显示是否已填、key 只显示末尾几位。
type NotifyView struct {
	BarkEnabled     bool     `json:"bark_enabled"`
	BarkServer      string   `json:"bark_server"`
	BarkGroup       string   `json:"bark_group"`
	BarkKeysMasked  []string `json:"bark_keys_masked"`
	FeishuEnabled   bool     `json:"feishu_enabled"`
	FeishuHookSet   bool     `json:"feishu_hook_set"`
	FeishuSecretSet bool     `json:"feishu_secret_set"`
	PanelURL        string   `json:"panel_url"`
}

// NotifyInput 是保存的内容。指针为空表示不改（敏感项）。
type NotifyInput struct {
	BarkEnabled   bool      `json:"bark_enabled"`
	BarkServer    string    `json:"bark_server"`
	BarkGroup     string    `json:"bark_group"`
	BarkKeys      *[]string `json:"bark_keys"`
	FeishuEnabled bool      `json:"feishu_enabled"`
	FeishuHook    *string   `json:"feishu_hook"`
	FeishuSecret  *string   `json:"feishu_secret"`
	PanelURL      string    `json:"panel_url"`
}

func (s *Server) loadNotify(ctx context.Context) (notifySetting, notifySecrets, error) {
	var st notifySetting
	var sec notifySecrets
	if err := s.loadSetting(ctx, notifyKey, &st); err != nil {
		return st, sec, err
	}
	if st.Sealed != "" {
		raw, err := base64.StdEncoding.DecodeString(st.Sealed)
		if err != nil {
			return st, sec, err
		}
		plain, err := s.Secrets.Open(raw, notifyAAD)
		if err != nil {
			return st, sec, err
		}
		if err := json.Unmarshal(plain, &sec); err != nil {
			return st, sec, err
		}
	}
	return st, sec, nil
}

func mask(k string) string {
	r := []rune(k)
	if len(r) <= 4 {
		return "••••"
	}
	return "••••" + string(r[len(r)-4:])
}

// Notify 返回推送配置。
func (s *Server) Notify(ctx context.Context) (NotifyView, error) {
	st, sec, err := s.loadNotify(ctx)
	if err != nil {
		return NotifyView{}, err
	}
	v := NotifyView{BarkEnabled: st.BarkEnabled, BarkServer: st.BarkServer, BarkGroup: st.BarkGroup, BarkKeysMasked: []string{},
		FeishuEnabled: st.FeishuEnabled, FeishuHookSet: sec.FeishuHook != "", FeishuSecretSet: sec.FeishuSecret != "", PanelURL: st.PanelURL}
	for _, k := range sec.BarkKeys {
		v.BarkKeysMasked = append(v.BarkKeysMasked, mask(k))
	}
	return v, nil
}

// mergeNotify 把提交的内容合并到已保存的配置（不落盘）。
func (s *Server) mergeNotify(ctx context.Context, in NotifyInput) (notifySetting, notifySecrets, error) {
	_, sec, err := s.loadNotify(ctx)
	if err != nil {
		return notifySetting{}, sec, err
	}
	if in.BarkKeys != nil {
		keys := []string{}
		for _, k := range *in.BarkKeys {
			// 允许直接粘贴 https://api.day.app/<key>/… 这样的推送地址。
			k = strings.TrimSpace(k)
			if i := strings.Index(k, "://"); i >= 0 {
				parts := strings.Split(strings.Trim(k[i+3:], "/"), "/")
				if len(parts) >= 2 {
					k = parts[1]
				}
			}
			if k != "" {
				keys = append(keys, k)
			}
		}
		sec.BarkKeys = keys
	}
	if in.FeishuHook != nil {
		sec.FeishuHook = strings.TrimSpace(*in.FeishuHook)
	}
	if in.FeishuSecret != nil {
		sec.FeishuSecret = strings.TrimSpace(*in.FeishuSecret)
	}
	st := notifySetting{BarkEnabled: in.BarkEnabled, BarkServer: strings.TrimRight(strings.TrimSpace(in.BarkServer), "/"),
		BarkGroup: strings.TrimSpace(in.BarkGroup), FeishuEnabled: in.FeishuEnabled, PanelURL: strings.TrimSpace(in.PanelURL)}
	switch {
	case st.BarkEnabled && len(sec.BarkKeys) == 0:
		return st, sec, &InputError{"启用 Bark 需要至少一个设备 key"}
	case st.FeishuEnabled && !strings.HasPrefix(sec.FeishuHook, "https://"):
		return st, sec, &InputError{"启用飞书需要填写机器人的 Webhook 地址（https://…）"}
	case st.BarkServer != "" && !strings.HasPrefix(st.BarkServer, "http"):
		return st, sec, &InputError{"Bark 服务器地址应以 https:// 开头"}
	}
	return st, sec, nil
}

// SaveNotify 保存推送配置。
func (s *Server) SaveNotify(ctx context.Context, in NotifyInput) error {
	st, sec, err := s.mergeNotify(ctx, in)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(sec)
	sealed, err := s.Secrets.Seal(b, notifyAAD)
	if err != nil {
		return err
	}
	st.Sealed = base64.StdEncoding.EncodeToString(sealed)
	return s.saveSetting(ctx, notifyKey, st)
}

func senders(st notifySetting, sec notifySecrets) []notify.Sender {
	out := []notify.Sender{}
	if st.BarkEnabled && len(sec.BarkKeys) > 0 {
		group := st.BarkGroup
		if group == "" {
			group = "Banbu Connect"
		}
		out = append(out, &notify.Bark{Server: st.BarkServer, DeviceKeys: sec.BarkKeys, Group: group})
	}
	if st.FeishuEnabled && sec.FeishuHook != "" {
		out = append(out, &notify.Feishu{Webhook: sec.FeishuHook, Secret: sec.FeishuSecret})
	}
	return out
}

// sendAll 推送到所有渠道，返回各渠道的失败原因（空表示全部成功）。
func sendAll(ctx context.Context, list []notify.Sender, m notify.Message) string {
	fails := []string{}
	for _, sd := range list {
		if err := sd.Send(ctx, m); err != nil {
			fails = append(fails, sd.Name()+"："+err.Error())
		}
	}
	return strings.Join(fails, "；")
}

// TestNotify 用页面上填的配置（未保存）发一条测试消息。
func (s *Server) TestNotify(ctx context.Context, in NotifyInput) (string, error) {
	st, sec, err := s.mergeNotify(ctx, in)
	if err != nil {
		return "", err
	}
	list := senders(st, sec)
	if len(list) == 0 {
		return "", &InputError{"请先启用至少一个推送渠道"}
	}
	m := notify.Message{Title: "Banbu Connect 测试消息", Body: "收到这条消息说明推送配置正确。", URL: st.PanelURL}
	res := sendAll(ctx, list, m)
	s.logNotify(ctx, "test", m, res)
	return res, nil
}

// Send 推送一条提醒并记录；没有启用任何渠道时只记录。
func (s *Server) Send(ctx context.Context, kind string, m notify.Message) {
	st, sec, err := s.loadNotify(ctx)
	if err != nil {
		slog.Warn("load notify config failed", "err", err)
		return
	}
	if m.URL == "" {
		m.URL = st.PanelURL
	}
	list := senders(st, sec)
	res := "没有启用推送渠道"
	if len(list) > 0 {
		res = sendAll(ctx, list, m)
	}
	if res != "" {
		slog.Warn("notify failed", "kind", kind, "title", m.Title, "result", res)
	}
	s.logNotify(ctx, kind, m, res)
}

func (s *Server) logNotify(ctx context.Context, kind string, m notify.Message, result string) {
	_, _ = s.DB.ExecContext(ctx, `INSERT INTO notify_log (kind, title, body, result) VALUES (?, ?, ?, ?)`, kind, m.Title, m.Body, result)
	_, _ = s.DB.ExecContext(ctx, `DELETE FROM notify_log WHERE id <= (SELECT id FROM notify_log ORDER BY id DESC LIMIT 1 OFFSET ?)`, maxLog)
}

// NotifyRecord 是一条推送记录。
type NotifyRecord struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Result    string `json:"result"`
	CreatedAt string `json:"created_at"`
}

// NotifyLog 返回最近的推送记录。
func (s *Server) NotifyLog(ctx context.Context) ([]NotifyRecord, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, kind, title, body, result, created_at FROM notify_log ORDER BY id DESC LIMIT ?`, maxLog)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NotifyRecord{}
	for rows.Next() {
		var r NotifyRecord
		if err := rows.Scan(&r.ID, &r.Kind, &r.Title, &r.Body, &r.Result, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Server) notifyAdminRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, log *slog.Logger) {
	fail := func(w http.ResponseWriter, err error) { writeErr(w, err, log) }
	mux.Handle("GET /api/v1/dist/notify", authed(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Notify(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}))
	mux.Handle("PUT /api/v1/dist/notify", authed(func(w http.ResponseWriter, r *http.Request) {
		var in NotifyInput
		if !decode(w, r, &in) {
			return
		}
		if err := s.SaveNotify(r.Context(), in); err != nil {
			fail(w, err)
			return
		}
		v, err := s.Notify(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}))
	mux.Handle("POST /api/v1/dist/notify/test", authed(func(w http.ResponseWriter, r *http.Request) {
		var in NotifyInput
		if !decode(w, r, &in) {
			return
		}
		res, err := s.TestNotify(r.Context(), in)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": res == "", "error": res})
	}))
	mux.Handle("GET /api/v1/dist/notify/log", authed(func(w http.ResponseWriter, r *http.Request) {
		list, err := s.NotifyLog(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}))
}
