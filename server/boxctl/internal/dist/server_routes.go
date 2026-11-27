package dist

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"boxctl/internal/objstore"
)

// AdminRoutes 注册管理页面用的接口，authed 负责登录和 CSRF 校验。
func (s *Server) AdminRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, log *slog.Logger) {
	fail := func(w http.ResponseWriter, err error) { writeErr(w, err, log) }

	mux.Handle("GET /api/v1/dist/draft", authed(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.Draft(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		cur, ok, err := s.Current(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		out := map[string]any{"draft": summarize(p), "current": nil}
		if ok {
			out["current"] = summarize(cur)
			// 草稿和已发布的内容不同，提示需要发布。
			cur.Version, cur.PublishedAt, cur.Note = 0, time.Time{}, ""
			a, _ := json.Marshal(cur)
			b, _ := json.Marshal(p)
			out["changed"] = string(a) != string(b)
		} else {
			out["changed"] = true
		}
		writeJSON(w, http.StatusOK, out)
	}))
	mux.Handle("GET /api/v1/dist/settings", authed(func(w http.ResponseWriter, r *http.Request) {
		sel, err := s.Selections(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		rules, err := s.Rules(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		egress, err := s.StrictEgress(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		boxSecret, err := s.BoxSecret(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		// 路由盒口令只给管理员看：没绑定管理服务的路由盒要在 LuCI 里填同一个。
		writeJSON(w, http.StatusOK, map[string]any{"selections": sel, "rules": rules, "strict_egress": egress, "box_secret": boxSecret})
	}))
	mux.Handle("PUT /api/v1/dist/settings", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Selections *map[string]string `json:"selections"`
			Rules      *[]string          `json:"rules"`
			Egress     *[]string          `json:"strict_egress"`
			BoxSecret  *string            `json:"box_secret"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.Selections != nil {
			if err := s.SetSelections(r.Context(), *in.Selections); err != nil {
				fail(w, err)
				return
			}
		}
		if in.Rules != nil {
			if err := s.SetRules(r.Context(), *in.Rules); err != nil {
				fail(w, err)
				return
			}
		}
		if in.Egress != nil {
			if err := s.SetStrictEgress(r.Context(), *in.Egress); err != nil {
				fail(w, err)
				return
			}
		}
		if in.BoxSecret != nil {
			if err := s.SetBoxSecret(r.Context(), *in.BoxSecret); err != nil {
				fail(w, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	mux.Handle("GET /api/v1/dist/profiles", authed(func(w http.ResponseWriter, r *http.Request) {
		h, err := s.History(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, h)
	}))
	mux.Handle("POST /api/v1/dist/publish", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Note string `json:"note"`
		}
		if !decode(w, r, &in) {
			return
		}
		p, err := s.Publish(r.Context(), in.Note)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, summarize(p))
	}))

	mux.Handle("GET /api/v1/dist/devices", authed(func(w http.ResponseWriter, r *http.Request) {
		d, err := s.Devices(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, d)
	}))
	mux.Handle("POST /api/v1/dist/devices", authed(func(w http.ResponseWriter, r *http.Request) {
		d, token, err := s.CreateDevice(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"device": d, "token": token})
	}))
	idAction := func(pattern string, f func(*http.Request, int64) error) {
		mux.Handle(pattern, authed(func(w http.ResponseWriter, r *http.Request) {
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				fail(w, ErrNotFound)
				return
			}
			if err := f(r, id); err != nil {
				fail(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		}))
	}
	// 修改名称：name 为空时恢复自动命名（用客户端上报的电脑名）。
	mux.Handle("PUT /api/v1/dist/devices/{id}/name", authed(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			fail(w, ErrNotFound)
			return
		}
		var in struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
			fail(w, &InputError{"请求内容无效"})
			return
		}
		d, err := s.RenameDevice(r.Context(), id, in.Name)
		if err != nil {
			fail(w, err)
			return
		}
		log.Info("客户端改名", "device", id, "name", d.Name, "custom", d.NameCustom)
		writeJSON(w, http.StatusOK, d)
	}))
	idAction("POST /api/v1/dist/devices/{id}/revoke", func(r *http.Request, id int64) error { return s.RevokeDevice(r.Context(), id) })
	idAction("DELETE /api/v1/dist/devices/{id}", func(r *http.Request, id int64) error { return s.DeleteDevice(r.Context(), id) })
	s.claudeAdminRoutes(mux, authed, log)
	s.geminiAdminRoutes(mux, authed, log)
	s.telemetryAdminRoutes(mux, authed, log)
	s.updatePolicyAdminRoutes(mux, authed, log)
	s.diagnosticsAdminRoutes(mux, authed, log)
	s.otelAdminRoutes(mux, authed, log)
	s.ruleSetRoutes(mux, authed, log)
	s.ruleProfileRoutes(mux, authed, log)
	s.simulateRoutes(mux, authed, log)
	s.enrollAdminRoutes(mux, authed, log)
	s.storageAdminRoutes(mux, authed, log)
	s.fileAdminRoutes(mux, authed, log)
	s.notifyAdminRoutes(mux, authed, log)
	s.alertAdminRoutes(mux, authed, log)
	s.aiOnlineAdminRoutes(mux, authed, log)
	s.accessAdminRoutes(mux, authed, log)

	mux.Handle("GET /api/v1/dist/releases", authed(func(w http.ResponseWriter, r *http.Request) {
		list, err := s.Releases(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"releases": list, "targets": Targets})
	}))
	mux.Handle("POST /api/v1/dist/releases", authed(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxReleaseSize+(1<<20))
		mr, err := r.MultipartReader()
		if err != nil {
			fail(w, &InputError{"请用表单上传"})
			return
		}
		fields := map[string]string{}
		var rel Release
		uploaded := false
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				fail(w, &InputError{"上传中断：" + err.Error()})
				return
			}
			switch part.FormName() {
			case "file":
				// 文件必须放在表单最后：前面的字段已经读到。
				rel, err = s.AddRelease(r.Context(), fields["version"], fields["target"], fields["notes"], part.FileName(), fields["signature"], part)
				if err != nil {
					fail(w, err)
					return
				}
				uploaded = true
			default:
				b, _ := io.ReadAll(io.LimitReader(part, 64<<10))
				fields[part.FormName()] = string(b)
			}
		}
		if !uploaded {
			fail(w, &InputError{"缺少更新包文件"})
			return
		}
		writeJSON(w, http.StatusOK, rel)
	}))
	mux.Handle("POST /api/v1/dist/releases/{id}/publish", authed(func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		var in struct {
			Published bool `json:"published"`
		}
		if !decode(w, r, &in) {
			return
		}
		if err := s.SetReleasePublished(r.Context(), id, in.Published); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	mux.Handle("PUT /api/v1/dist/releases/{id}/devices", authed(func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		var in struct {
			Devices []int64 `json:"devices"`
		}
		if !decode(w, r, &in) {
			return
		}
		if err := s.SetReleaseDevices(r.Context(), id, in.Devices); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	idAction("DELETE /api/v1/dist/releases/{id}", func(r *http.Request, id int64) error { return s.DeleteRelease(r.Context(), id) })
}

// ProfileSummary 是给管理页面看的配置摘要，不含节点凭据。
type ProfileSummary struct {
	Version     int64             `json:"version"`
	PublishedAt *time.Time        `json:"published_at,omitempty"`
	Note        string            `json:"note"`
	Nodes       []string          `json:"nodes"`
	Groups      []GroupSummary    `json:"groups"`
	Selections  map[string]string `json:"selections"`
	Rules       int               `json:"rules"`
	// RuleSets 是下发的规则集条数；-1 表示没有下发、客户端用内置默认。
	RuleSets int `json:"rule_sets"`
	// Variants 是方案（ADR-062、ADR-064）：分配了的客户端整份替换节点、代理组、线路选择、规则和远程规则集。
	Variants []VariantSummary `json:"variants"`
	// ProfileVersion 是「默认」方案的版本号。
	ProfileVersion string `json:"profile_version"`
}

type VariantSummary struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Devices  int    `json:"devices"`
	Nodes    int    `json:"nodes"`
	Groups   int    `json:"groups"`
	Rules    int    `json:"rules"`
	RuleSets int    `json:"rule_sets"` // -1 表示客户端内置默认
}

type GroupSummary struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Strict  bool     `json:"strict"`
	Members []string `json:"members"`
}

func summarize(p Profile) ProfileSummary {
	s := ProfileSummary{Version: p.Version, ProfileVersion: p.ProfileVersion, Note: p.Note, Selections: p.Selections, Rules: len(p.Rules), RuleSets: len(p.RuleSets), Nodes: []string{}, Groups: []GroupSummary{}}
	if p.RuleSets == nil {
		s.RuleSets = -1
	}
	s.Variants = []VariantSummary{}
	for _, v := range p.Variants {
		vs := VariantSummary{Name: v.Name, Version: v.Version, Devices: len(v.Devices), Nodes: len(v.Nodes), Groups: len(v.Groups), Rules: len(v.Rules), RuleSets: len(v.RuleSets)}
		if !v.Scoped {
			vs.Nodes, vs.Groups = len(p.Nodes), len(p.Groups)
		}
		if v.RuleSets == nil {
			vs.RuleSets = -1
		}
		s.Variants = append(s.Variants, vs)
	}
	if !p.PublishedAt.IsZero() {
		t := p.PublishedAt
		s.PublishedAt = &t
	}
	if s.Selections == nil {
		s.Selections = map[string]string{}
	}
	for _, n := range p.Nodes {
		name, _ := n["name"].(string)
		s.Nodes = append(s.Nodes, name)
	}
	for _, g := range p.Groups {
		members := g.Members
		if members == nil {
			members = []string{}
		}
		s.Groups = append(s.Groups, GroupSummary{Name: g.Name, Type: g.Type, Strict: g.Strict, Members: members})
	}
	return s
}

// ---- 客户端接口：只认客户端令牌 ----

// 客户端随请求报告自己的版本和平台。
const (
	headerAppVersion = "X-Banbu-Version"
	headerPlatform   = "X-Banbu-Platform"
	// 设备名和系统版本可能含中文，客户端做 URL 编码（url.QueryEscape）后放进请求头。
	headerHostname  = "X-Banbu-Hostname"
	headerOSVersion = "X-Banbu-OS"
)

// clientInfo 读取客户端在请求头里上报的信息。
func clientInfo(r *http.Request) ClientInfo {
	header := func(name string, max int) string {
		v, err := url.QueryUnescape(r.Header.Get(name))
		if err != nil {
			return ""
		}
		v = strings.TrimSpace(strings.ToValidUTF8(v, ""))
		if r := []rune(v); len(r) > max {
			v = string(r[:max])
		}
		return v
	}
	return ClientInfo{
		AppVersion: header(headerAppVersion, 32),
		Platform:   header(headerPlatform, 32),
		Hostname:   header(headerHostname, 64),
		OSVersion:  header(headerOSVersion, 64),
	}
}

// ClientRoutes 注册客户端拉取配置和更新包的接口。
func (s *Server) ClientRoutes(mux *http.ServeMux, log *slog.Logger) {
	device := func(fn func(http.ResponseWriter, *http.Request, Device)) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || token == "" {
				writeJSON(w, http.StatusUnauthorized, apiError{"unauthenticated", "缺少客户端令牌"})
				return
			}
			d, err := s.Authenticate(r.Context(), token)
			if errors.Is(err, ErrNotFound) {
				writeJSON(w, http.StatusUnauthorized, apiError{"unauthenticated", "客户端令牌无效或已吊销"})
				return
			}
			if err != nil {
				writeErr(w, err, log)
				return
			}
			fn(w, r, d)
		})
	}
	s.claudeClientRoutes(mux, device, log)
	s.geminiClientRoutes(mux, device, log)
	s.aiOnlineClientRoutes(mux, device, log)
	s.accessClientRoutes(mux, device, log)
	s.connClientRoutes(mux, device, func(w http.ResponseWriter, err error) { writeErr(w, err, log) })
	s.connLiveClientRoutes(mux, device)
	s.updateClientRoutes(mux, device, log)
	s.diagnosticsClientRoutes(mux, device, log)
	s.otelClientRoutes(mux, device, log)
	s.enrollClientRoutes(mux, log)
	mux.Handle("GET /api/v1/client/profile", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		have, _ := strconv.ParseInt(strings.Trim(r.Header.Get("If-None-Match"), `"v`), 10, 64)
		p, ok, err := s.Current(r.Context())
		if err != nil {
			writeErr(w, err, log)
			return
		}
		if ok {
			have = p.Version // 304 时客户端已有这一版，200 时马上就会拿到
		}
		_ = s.Touch(r.Context(), d.ID, remoteIP(r), clientInfo(r), have)
		if !ok {
			writeJSON(w, http.StatusNotFound, apiError{"no_profile", "服务端还没有发布配置"})
			return
		}
		etag := fmt.Sprintf(`"v%d"`, p.Version)
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "no-store")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		writeJSON(w, http.StatusOK, p.ForDevice(d.ID)) // 按客户端换成它的规则方案（ADR-062）
	}))
	// 更新检查：与 Tauri updater 的动态服务格式相同，没有更新时返回 204。
	mux.Handle("GET /api/v1/client/update", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		q := r.URL.Query()
		target := q.Get("target") + "-" + q.Get("arch")
		current := q.Get("current_version")
		info := clientInfo(r)
		if current != "" {
			info.AppVersion = current // 外壳（应用）的版本，以它为准
		}
		_ = s.Touch(r.Context(), d.ID, remoteIP(r), info, 0)
		rel, ok, err := s.LatestRelease(r.Context(), target, d.ID)
		if err != nil {
			writeErr(w, err, log)
			return
		}
		if !ok || (current != "" && CompareVersions(rel.Version, current) <= 0) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mode, minVersion, err := s.updateMode(r.Context(), rel, target, d.ID, current)
		if err != nil {
			writeErr(w, err, log)
			return
		}
		writeJSON(w, http.StatusOK, UpdateInfo{
			Version: rel.Version, Mode: mode, MinVersion: minVersion, Notes: rel.Notes, PubDate: pubDate(rel.CreatedAt),
			ReleaseID: rel.ID, Signature: rel.Signature, Size: rel.Size,
		})
	}))
	mux.Handle("GET /api/v1/client/releases/{id}/download", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		rel, store, key, err := s.OpenRelease(r.Context(), id, d.ID)
		if err != nil {
			writeErr(w, err, log)
			return
		}
		s.serveObject(w, r, store, key, rel.FileName, log)
	}))
	s.fileClientRoutes(mux, device, log)
	s.localStore().Routes(mux)
}

// 签名下载地址的有效期：只用来立刻开始下载，过期后已开始的下载不受影响。
const signedGetTTL = 15 * time.Minute

// serveObject 把存储里的文件交给客户端。带 ?redirect=1 的新版客户端拿到 302 跳转，自己去签名地址下载
// （OSS 时不经过管理服务）；旧版客户端不认跳转（证书指纹只认管理服务），由管理服务读出来转发。
func (s *Server) serveObject(w http.ResponseWriter, r *http.Request, store objstore.Store, key, fileName string, log *slog.Logger) {
	if r.URL.Query().Get("redirect") == "1" {
		u, err := store.SignGet(r.Context(), key, fileName, signedGetTTL)
		if err != nil {
			writeErr(w, err, log)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, u.URL, http.StatusFound)
		return
	}
	rc, size, err := store.Open(r.Context(), key)
	if errors.Is(err, objstore.ErrNotFound) {
		writeErr(w, ErrNotFound, log)
		return
	}
	if err != nil {
		writeErr(w, err, log)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", objstore.ContentDisposition(fileName))
	if size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// UpdateInfo 是更新检查的结果。客户端服务把 ReleaseID 换成本机下载地址交给 Tauri updater。
type UpdateInfo struct {
	Version string `json:"version"`
	// Mode 是更新方式：normal / push / silent / force（客户端低于最低版本时）；旧版管理服务没有，按 normal。
	Mode       string `json:"mode,omitempty"`
	MinVersion string `json:"min_version,omitempty"`
	Notes      string `json:"notes"`
	PubDate    string `json:"pub_date"`
	ReleaseID  int64  `json:"release_id"`
	Signature  string `json:"signature"`
	Size       int64  `json:"size"`
}

func pubDate(created string) string {
	if t, err := time.Parse(time.RFC3339, created); err == nil {
		return t.Format(time.RFC3339)
	}
	if t, err := time.Parse("2006-01-02T15:04:05Z", created); err == nil {
		return t.Format(time.RFC3339)
	}
	return ""
}

func remoteIP(r *http.Request) string {
	// 部署在反向代理后面时，由代理设置 X-Real-IP。
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" && net.ParseIP(ip) != nil {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error, log *slog.Logger) {
	var in *InputError
	switch {
	case errors.As(err, &in):
		writeJSON(w, http.StatusBadRequest, apiError{"invalid", in.Msg})
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, apiError{"not_found", err.Error()})
	default:
		if log != nil {
			log.Error("dist", "err", err)
		}
		writeJSON(w, http.StatusInternalServerError, apiError{"failed", err.Error()})
	}
}

func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(out); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{"bad_request", "请求内容无效"})
		return false
	}
	return true
}
