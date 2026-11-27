package desktop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"runtime"
	"sync"
	"time"

	"boxctl/internal/buildinfo"
	"boxctl/internal/dist"
)

// 首页的应用更新：服务查有没有新版本；用户点「更新」后，服务记下请求，托盘外壳（每 3 秒读一次状态）
// 认领后用 Tauri updater 下载、校验、安装，并把进度回报回来；安装完由用户点「立即重启」，外壳再重启应用。
// 页面没有 Tauri 权限（见 client/README.md「安全」），下载安装只能由外壳做。
//
// S1.10（ADR-057）：服务在后台每 30 分钟检查一次，按管理服务给的更新方式处理：
//   - push：发系统通知（每个版本一次），用户到首页安装；
//   - silent：自动请求外壳安装，装好后请外壳在用户空闲时重启应用（下次退出、重启也会生效）；
//   - force（管理员发布为强制更新，或低于最低版本）：和 silent 一样后台自动安装、空闲时重启；
//     另外主界面下次打开时全屏拦住，自动安装并重启后才能继续使用（web/src/pages/desktop/ForceUpdate.tsx）。
// 重启只重启应用（托盘与窗口），保护服务靠系统持久配置，不受影响（ADR-011）。进度阶段变化时报给管理服务（BXC-F0-058）。

const (
	updateCheckTTL   = 10 * time.Minute
	updateCheckEvery = 30 * time.Minute
	updateTick       = 15 * time.Second
	autoRetry        = time.Hour        // 自动安装失败后多久再试
	stuckProgress    = 30 * time.Minute // 进度这么久没变当作中断
)

// restartTTL 是重启请求的有效期。外壳每 3 秒读一次状态，足够它认领；过期后不再出现在状态里。
// 否则请求一直留着，重启后的外壳会再次看到它而再次重启：后台服务没更新（不重启）时就是死循环。
const restartTTL = 15 * time.Second

// AppUpdateRequest 是待外壳处理的请求（出现在状态里）。
type AppUpdateRequest struct {
	ID     string `json:"id"`
	Action string `json:"action"` // install / restart / restart_when_idle（静默更新：用户空闲时重启）
}

// AppUpdateProgress 是外壳回报的进度。
type AppUpdateProgress struct {
	ID         string    `json:"id"`
	Phase      string    `json:"phase"` // requested / checking / downloading / installing / installed / failed / none（没有可用更新）
	Version    string    `json:"version,omitempty"`
	Downloaded int64     `json:"downloaded"`
	Total      int64     `json:"total"` // 0 表示未知
	Error      string    `json:"error,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// AppUpdateStatus 是首页看到的。
type AppUpdateStatus struct {
	Current   string             `json:"current"`
	Bound     bool               `json:"bound"`
	Available *dist.UpdateInfo   `json:"available"`
	CheckedAt *time.Time         `json:"checked_at,omitempty"`
	CheckErr  string             `json:"check_error,omitempty"`
	Progress  *AppUpdateProgress `json:"progress"`
	// Auto 表示这次安装是按静默或强制更新自动发起的。
	Auto bool `json:"auto,omitempty"`
}

type appUpdater struct {
	client *dist.Client
	report updateReporter
	notify func(title, body string) // 系统通知（经状态里的通知交给外壳）

	mu        sync.Mutex
	available *dist.UpdateInfo
	checkedAt time.Time
	checkErr  string
	request   *AppUpdateRequest
	requested time.Time
	progress  *AppUpdateProgress
	now       func() time.Time // 测试用；nil 时用 time.Now

	appVersion    string // 应用（外壳）的版本：Tauri updater 检查更新时带来，服务与应用可能不同版
	notified      string // 已为哪个版本发过推送通知
	autoInstall   bool   // 当前安装是自动发起的
	autoVersion   string
	autoTried     time.Time
	idleRequested bool   // 已请外壳空闲时重启
	reported      string // 最近一次报给管理服务的「版本/阶段」
}

func (u *appUpdater) clock() time.Time {
	if u.now != nil {
		return u.now()
	}
	return time.Now()
}

// updaterTarget 是 Tauri updater 的 {{target}}/{{arch}}：服务与应用同平台同架构。
func updaterTarget() (string, string) {
	arch := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}[runtime.GOARCH]
	if arch == "" {
		arch = runtime.GOARCH
	}
	return runtime.GOOS, arch
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (u *appUpdater) check(ctx context.Context, force bool) {
	u.mu.Lock()
	fresh := !u.checkedAt.IsZero() && time.Since(u.checkedAt) < updateCheckTTL
	u.mu.Unlock()
	if fresh && !force {
		return
	}
	target, arch := updaterTarget()
	u.mu.Lock()
	current := u.appVersion
	u.mu.Unlock()
	if current == "" {
		current = buildinfo.Version
	}
	info, err := u.client.CheckUpdate(ctx, target, arch, current)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.checkedAt = time.Now()
	if err != nil {
		u.checkErr = err.Error()
		return
	}
	// 没有更新了（已装好并重启）或换了版本：之前的自动更新状态作废。
	if info == nil || u.available == nil || info.Version != u.available.Version {
		u.idleRequested = false
	}
	u.available, u.checkErr = info, ""
}

// SetAppVersion 记下应用（外壳）的版本：Tauri updater 检查更新时把它带到本机接口。
func (u *appUpdater) SetAppVersion(v string) {
	if v == "" {
		return
	}
	u.mu.Lock()
	u.appVersion = v
	u.mu.Unlock()
}

// Run 在后台定时检查更新并按更新方式处理（S1.10），直到 ctx 结束。
func (u *appUpdater) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Minute): // 启动后先等外壳和网络就绪
	}
	t := time.NewTicker(updateTick)
	defer t.Stop()
	var last time.Time
	for {
		if u.client.Status().Bound && (last.IsZero() || time.Since(last) >= updateCheckEvery) {
			u.check(ctx, true)
			last = time.Now()
		}
		u.auto()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func modeOf(a *dist.UpdateInfo) string {
	if a == nil || a.Mode == "" {
		return dist.UpdateNormal
	}
	return a.Mode
}

// auto 按更新方式推进：推送通知、自动安装、空闲重启。强制更新的全屏拦截在主界面里做。
func (u *appUpdater) auto() {
	now := u.clock()
	var notes [][2]string
	u.mu.Lock()
	a := u.available
	mode := modeOf(a)
	if a != nil {
		p := u.progress
		installed := p != nil && p.Phase == "installed" && p.Version == a.Version
		busy := p != nil && !installed && p.Phase != "failed" && p.Phase != "none" && now.Sub(p.UpdatedAt) < stuckProgress
		switch mode {
		case dist.UpdatePush:
			if u.notified != a.Version {
				u.notified = a.Version
				notes = append(notes, [2]string{"Banbu Connect 有新版本 " + a.Version, "打开 Banbu Connect 首页安装更新。"})
			}
		case dist.UpdateSilent, dist.UpdateForce:
			if !installed && !busy && (u.autoVersion != a.Version || now.Sub(u.autoTried) >= autoRetry) {
				u.autoVersion, u.autoTried = a.Version, now
				u.requestLocked("install")
				u.autoInstall = true
				if mode == dist.UpdateForce && u.notified != a.Version {
					u.notified = a.Version
					notes = append(notes, [2]string{"需要更新 Banbu Connect", "管理员要求更新到 " + a.Version + "，正在后台下载；下次打开主界面时完成更新。"})
				}
			}
			if installed && !u.idleRequested {
				u.idleRequested = true
				u.requestLocked("restart_when_idle")
			}
		}
	}
	u.mu.Unlock()
	if u.notify != nil {
		for _, n := range notes {
			u.notify(n[0], n[1])
		}
	}
}

func (u *appUpdater) status() AppUpdateStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := AppUpdateStatus{Current: buildinfo.Version, Bound: u.client.Status().Bound, Available: u.available, CheckErr: u.checkErr, Progress: u.progress,
		Auto: u.autoInstall}
	if u.appVersion != "" {
		st.Current = u.appVersion
	}
	if !u.checkedAt.IsZero() {
		t := u.checkedAt
		st.CheckedAt = &t
	}
	return st
}

// Pending 是状态里给外壳的请求；外壳按 ID 去重，处理过的不会再做。
func (u *appUpdater) Pending() *AppUpdateRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.request != nil && u.request.Action != "install" && u.clock().Sub(u.requested) > restartTTL {
		u.request = nil
	}
	return u.request
}

func (u *appUpdater) requestAction(action string) *AppUpdateProgress {
	u.mu.Lock()
	defer u.mu.Unlock()
	if action == "install" {
		u.autoInstall = false // 用户在首页点的
	}
	return u.requestLocked(action)
}

// requestLocked 记下给外壳的请求。调用方持有 u.mu。
func (u *appUpdater) requestLocked(action string) *AppUpdateProgress {
	u.request = &AppUpdateRequest{ID: newID(), Action: action}
	u.requested = u.clock()
	if action == "install" {
		u.progress = &AppUpdateProgress{ID: u.request.ID, Phase: "requested", UpdatedAt: time.Now()}
		if u.available != nil {
			u.progress.Version = u.available.Version
		}
	}
	return u.progress
}

func (u *appUpdater) setProgress(p AppUpdateProgress) {
	p.UpdatedAt = time.Now()
	u.mu.Lock()
	// 外壳从托盘发起的更新没有请求 ID，也照样显示；页面发起的只接受当前请求的进度。
	if u.request != nil && p.ID != "" && p.ID != u.request.ID && u.request.Action == "install" {
		u.mu.Unlock()
		return
	}
	u.progress = &p
	key := p.Version + "/" + p.Phase
	send := key != u.reported
	u.reported = key
	mode := modeOf(u.available)
	u.mu.Unlock()
	// 阶段变化时报给管理服务（下载中的进度不逐条报）。
	if send && u.client != nil && u.client.Status().Bound {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_ = u.client.ReportUpdateProgress(ctx, dist.UpdateProgressReport{Version: p.Version, Mode: mode, Phase: p.Phase, Error: p.Error})
		}()
	}
	if p.Phase == "failed" && u.report != nil {
		u.report("error", "应用更新失败："+clip(p.Error, 400), map[string]string{"stage": "app_update", "version": clip(p.Version, 32)})
	}
}

func (u *appUpdater) routes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /api/v1/desktop/app-update", authed(func(w http.ResponseWriter, r *http.Request) {
		u.check(r.Context(), r.URL.Query().Get("refresh") == "1")
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, u.status())
	}))
	mux.Handle("POST /api/v1/desktop/app-update/install", authed(func(w http.ResponseWriter, r *http.Request) {
		u.check(r.Context(), false)
		if u.status().Available == nil {
			writeJSON(w, http.StatusConflict, apiErr{"no_update", "没有可安装的更新"})
			return
		}
		u.requestAction("install")
		writeJSON(w, http.StatusOK, u.status())
	}))
	mux.Handle("POST /api/v1/desktop/app-update/restart", authed(func(w http.ResponseWriter, _ *http.Request) {
		if p := u.status().Progress; p == nil || p.Phase != "installed" {
			writeJSON(w, http.StatusConflict, apiErr{"not_installed", "更新还没有安装完成"})
			return
		}
		u.requestAction("restart")
		writeJSON(w, http.StatusOK, u.status())
	}))
	// 外壳回报进度。
	mux.Handle("POST /api/v1/desktop/app-update/progress", authed(func(w http.ResponseWriter, r *http.Request) {
		var p AppUpdateProgress
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&p); err != nil || p.Phase == "" {
			writeJSON(w, http.StatusBadRequest, apiErr{"bad_request", "请求内容无效"})
			return
		}
		p.Error = clip(p.Error, 1000)
		u.setProgress(p)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
}
