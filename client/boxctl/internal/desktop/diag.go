package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"boxctl/internal/core"
	"boxctl/internal/dist"
	"boxctl/internal/killswitch"
	"boxctl/internal/panel"
	"boxctl/internal/trust"
)

// 诊断数据：观察 Kill Switch、受信网络、严格线路和内核的状态，把出口探测结果和故障排进队列，
// 随同步上报给管理服务。收集范围见 internal/dist/diagnostics.go：只记排查连接问题需要的，
// 不记访问记录、Wi-Fi 名称、开关机、服务启动退出和用户操作。
//
// 只观察状态的变化，不在各模块里埋点：哪些东西会被上报，都在这一个文件里。

const (
	diagFile        = "diagnostics.json"
	diagObserve     = 3 * time.Second
	diagUploadEvery = 5 * time.Minute
	diagUploadSoon  = 30 * time.Second // 出现警告或错误后，最多等这么久就上报
	diagMaxQueue    = 1000             // 上报不出去时本机最多保留的事件，超出丢最旧的
	diagBatch       = 200
	// 出口探测每 10–30 秒一次，结果没变时每 15 分钟才记一条（带上这段时间探测和失败的次数）。
	diagEgressEvery = 15 * time.Minute
)

// diagSources 是被观察的状态。
type diagSources struct {
	killswitch func() killswitch.Status
	network    func() trust.Status
	strict     func() trust.StrictStatus
	core       func() core.CoreStatus
}

// DiagStatus 是本机诊断上报的状态，界面展示用，让用户看到会上报什么。
type DiagStatus struct {
	Enabled    bool              `json:"enabled"`
	Bound      bool              `json:"bound"`
	Pending    int               `json:"pending"`
	Records    []dist.DiagRecord `json:"records"` // 待上报的，最新的在前，最多 100 条
	Snapshot   dist.DiagSnapshot `json:"snapshot"`
	LastUpload *time.Time        `json:"last_upload,omitempty"`
	LastError  string            `json:"last_error"`
}

type diagnostics struct {
	path   string
	client *dist.Client
	src    diagSources
	bound  func() bool // 绑定了管理服务
	log    *slog.Logger
	now    func() time.Time
	soon   chan struct{}

	mu         sync.Mutex
	enabled    bool
	records    []dist.DiagRecord
	lastSeq    int64
	lastUpload time.Time
	lastErr    string

	upMu sync.Mutex // 串行化上报

	// 以下只在观察循环里使用。
	seen   bool
	ks     killswitch.Status
	core   core.CoreStatus
	net    dist.DiagNet
	egress egressSampler
	strict map[string]*egressSampler
}

type diagQueueFile struct {
	LastSeq int64             `json:"last_seq"`
	Records []dist.DiagRecord `json:"records"`
}

func newDiagnostics(dir string, client *dist.Client, src diagSources, enabled bool, log *slog.Logger) *diagnostics {
	d := &diagnostics{
		path: filepath.Join(dir, diagFile), client: client, src: src, log: log,
		now: time.Now, soon: make(chan struct{}, 1), enabled: enabled, strict: map[string]*egressSampler{},
		bound: func() bool { return client != nil && client.Status().Bound },
	}
	var q diagQueueFile
	if b, err := os.ReadFile(d.path); err == nil && json.Unmarshal(b, &q) == nil {
		d.records, d.lastSeq = q.Records, q.LastSeq
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) && log != nil {
		log.Warn("读取诊断数据队列失败", "err", err)
	}
	return d
}

// active 表示现在收集：绑定了管理服务，且没有关闭诊断上报。
func (d *diagnostics) active() bool {
	d.mu.Lock()
	on := d.enabled
	d.mu.Unlock()
	return on && d.bound()
}

// SetEnabled 打开或关闭诊断上报。关闭时清空本机还没上报的事件。
func (d *diagnostics) SetEnabled(on bool) {
	d.mu.Lock()
	d.enabled = on
	if !on {
		d.records = nil
	}
	d.saveLocked()
	d.mu.Unlock()
}

func (d *diagnostics) add(kind, level, msg string, fields map[string]string) {
	if !d.active() {
		return
	}
	now := d.now()
	d.mu.Lock()
	seq := now.UnixNano()
	if seq <= d.lastSeq {
		seq = d.lastSeq + 1
	}
	d.lastSeq = seq
	d.records = append(d.records, dist.DiagRecord{Seq: seq, Time: now.UTC(), Kind: kind, Level: level, Message: msg, Fields: fields})
	if len(d.records) > diagMaxQueue {
		d.records = d.records[len(d.records)-diagMaxQueue:]
	}
	d.saveLocked()
	d.mu.Unlock()
	if level != "info" {
		select {
		case d.soon <- struct{}{}:
		default:
		}
	}
}

func (d *diagnostics) saveLocked() {
	b, err := json.Marshal(diagQueueFile{LastSeq: d.lastSeq, Records: d.records})
	if err == nil {
		tmp := d.path + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, d.path)
		}
	}
	if err != nil && d.log != nil {
		d.log.Warn("保存诊断数据队列失败", "err", err)
	}
}

// Run 观察状态并定时上报。
func (d *diagnostics) Run(ctx context.Context) {
	observe := time.NewTicker(diagObserve)
	defer observe.Stop()
	upload := time.NewTimer(time.Minute) // 启动后先等一会儿，内核和网络检查都跑过一轮
	defer upload.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-observe.C:
			d.observe()
		case <-d.soon:
			if !upload.Stop() {
				select {
				case <-upload.C:
				default:
				}
			}
			upload.Reset(diagUploadSoon)
		case <-upload.C:
			if err := d.Upload(ctx); err != nil && d.log != nil {
				d.log.Info("上报诊断数据失败", "err", err)
			}
			upload.Reset(diagUploadEvery)
		}
	}
}

// Upload 上报快照和排队的事件，成功后从队列移除。
func (d *diagnostics) Upload(ctx context.Context) error {
	if !d.active() {
		return nil
	}
	d.upMu.Lock()
	defer d.upMu.Unlock()
	for range diagMaxQueue/diagBatch + 1 {
		d.mu.Lock()
		batch := append([]dist.DiagRecord{}, d.records[:min(len(d.records), diagBatch)]...)
		d.mu.Unlock()
		_, err := d.client.Report(ctx, dist.DiagReport{Snapshot: d.snapshot(), Records: batch})
		d.mu.Lock()
		if err != nil {
			d.lastErr = err.Error()
			d.mu.Unlock()
			return err
		}
		d.lastErr, d.lastUpload = "", d.now().UTC()
		if len(batch) > 0 {
			last := batch[len(batch)-1].Seq
			i := 0
			for i < len(d.records) && d.records[i].Seq <= last {
				i++
			}
			d.records = d.records[i:]
			d.saveLocked()
		}
		more := len(d.records) > 0
		d.mu.Unlock()
		if !more {
			return nil
		}
	}
	return nil
}

func (d *diagnostics) Status() DiagStatus {
	d.mu.Lock()
	s := DiagStatus{Enabled: d.enabled, Pending: len(d.records), LastError: d.lastErr, Records: []dist.DiagRecord{}}
	for i := len(d.records) - 1; i >= 0 && len(s.Records) < 100; i-- {
		s.Records = append(s.Records, d.records[i])
	}
	if !d.lastUpload.IsZero() {
		t := d.lastUpload
		s.LastUpload = &t
	}
	d.mu.Unlock()
	s.Bound = d.bound()
	s.Snapshot = d.snapshot()
	return s
}

// ---- 观察 ----

func (d *diagnostics) observe() {
	ks := d.src.killswitch()
	core := d.src.core()
	ns := d.src.network()
	ss := d.src.strict()
	net := diagNet(ns)
	if !d.seen {
		// 第一次只记下状态：服务刚启动时的状态变化不是故障，也不记录服务何时启动。
		d.seen, d.ks, d.core, d.net = true, ks, core, net
		d.egress.prime(ns.Egress)
		for _, g := range ss.Groups {
			s := &egressSampler{}
			s.prime(g.Egress)
			d.strict[g.Group] = s
		}
		return
	}

	if ks.State != d.ks.State {
		d.add("killswitch", stateLevel(ks.State), fmt.Sprintf("保护服务：%s → %s", stateName(d.ks.State), stateName(ks.State)),
			map[string]string{"from": d.ks.State, "to": ks.State, "path": string(ks.Verdict.Path), "reason": redact(ks.Verdict.Reason)})
	}
	if ks.LastError != "" && ks.LastError != d.ks.LastError {
		d.add("killswitch", "error", "系统规则出错："+ks.LastError, nil)
	}
	// 内核预期在运行时的故障才记；用户自己停掉代理不记。
	if core.Desired && d.core.Desired {
		if ks.Health.DNSOK != d.ks.Health.DNSOK && !ks.Health.CheckedAt.IsZero() {
			if ks.Health.DNSOK {
				d.add("health", "info", "Mihomo DNS 已恢复", nil)
			} else {
				d.add("health", "warn", "Mihomo DNS 不可用", map[string]string{"detail": ks.Health.DNSDetail})
			}
		}
		if core.TUN && ks.Health.TUNOK != d.ks.Health.TUNOK {
			if ks.Health.TUNOK {
				d.add("health", "info", "TUN 已接管流量", map[string]string{"interface": ks.Health.Interface})
			} else {
				d.add("health", "warn", "TUN 未接管流量", map[string]string{"interface": ks.Health.Interface})
			}
		}
		if d.core.Running && !core.Running {
			d.add("core", "error", "代理内核意外退出", map[string]string{"error": core.LastError})
		}
	}
	if core.LastError != "" && core.LastError != d.core.LastError {
		d.add("core", "error", "应用配置失败："+core.LastError, nil)
	}
	if core.TUNError != "" && core.TUNError != d.core.TUNError {
		d.add("core", "warn", "TUN 启动失败："+core.TUNError, nil)
	}
	if net != d.net {
		d.add("network", "info", fmt.Sprintf("网络变化：%s → %s", netName(d.net), netName(net)), netFields(net))
	}

	if eg, n, failed, ok := d.egress.next(ns.Egress, d.now()); ok {
		f := egressFields(eg, n, failed)
		d.add("egress", egressLevel(eg), egressMessage("受信 Wi-Fi 出口", eg), f)
	}
	groups := map[string]*egressSampler{}
	for _, g := range ss.Groups {
		s := d.strict[g.Group]
		if s == nil {
			s = &egressSampler{}
		}
		groups[g.Group] = s
		if eg, n, failed, ok := s.next(g.Egress, d.now()); ok {
			f := egressFields(eg, n, failed)
			f["group"] = g.Group
			f["allowed"] = strconv.FormatBool(g.Allowed)
			f["required"] = strconv.FormatBool(ss.Required)
			if g.Reason != "" {
				f["reason"] = g.Reason
			}
			d.add("strict_egress", egressLevel(eg), egressMessage("严格线路「"+g.Group+"」出口", eg), f)
		}
	}
	d.strict = groups
	d.ks, d.core, d.net = ks, core, net
}

func (d *diagnostics) snapshot() dist.DiagSnapshot {
	ks := d.src.killswitch()
	core := d.src.core()
	ns := d.src.network()
	ss := d.src.strict()
	s := dist.DiagSnapshot{
		Time: d.now().UTC(), KillSwitch: ks.State, Path: string(ks.Verdict.Path), Reason: redact(ks.Verdict.Reason),
		DNSOK: ks.Health.DNSOK, TUNOK: ks.Health.TUNOK,
		CoreRunning: core.Running, CoreError: core.LastError, TUNError: core.TUNError,
		Network: diagNet(ns),
	}
	if ns.Probing && !ns.Egress.CheckedAt.IsZero() {
		e := diagEgress(ns.Egress)
		s.Egress = &e
	}
	for _, g := range ss.Groups {
		s.Strict = append(s.Strict, dist.DiagStrict{Group: g.Group, Allowed: g.Allowed, Egress: diagEgress(g.Egress)})
	}
	return s
}

// egressSampler 决定哪些探测结果要记：结果变了（成败、IP、地区、错误）就记，没变时每 15 分钟记一条。
type egressSampler struct {
	checked time.Time // 最近看到的一次探测
	last    dist.DiagEgress
	at      time.Time
	has     bool
	probes  int // 上次记录以来的探测次数
	failed  int
}

func (s *egressSampler) prime(e trust.Egress) { s.checked = e.CheckedAt }

func (s *egressSampler) next(e trust.Egress, now time.Time) (eg dist.DiagEgress, probes, failed int, ok bool) {
	if e.CheckedAt.IsZero() || e.CheckedAt.Equal(s.checked) {
		return eg, 0, 0, false
	}
	s.checked = e.CheckedAt
	eg = diagEgress(e)
	s.probes++
	if !eg.OK {
		s.failed++
	}
	changed := !s.has || eg.OK != s.last.OK || eg.IP != s.last.IP || eg.Country != s.last.Country || eg.Error != s.last.Error
	if !changed && now.Sub(s.at) < diagEgressEvery {
		return eg, 0, 0, false
	}
	probes, failed = s.probes, s.failed
	s.last, s.at, s.has, s.probes, s.failed = eg, now, true, 0, 0
	return eg, probes, failed, true
}

func diagEgress(e trust.Egress) dist.DiagEgress {
	return dist.DiagEgress{OK: e.OK, IP: e.IP, Country: e.Country, Error: e.Error, LatencyMS: e.LatencyMS, CheckedAt: e.CheckedAt}
}

func egressFields(e dist.DiagEgress, probes, failed int) map[string]string {
	f := map[string]string{
		"ok": strconv.FormatBool(e.OK), "latency_ms": strconv.FormatInt(e.LatencyMS, 10),
		"probes": strconv.Itoa(probes), "failed": strconv.Itoa(failed),
	}
	if e.IP != "" {
		f["ip"] = e.IP
	}
	if e.Country != "" {
		f["country"] = e.Country
	}
	if e.Error != "" {
		f["error"] = e.Error
	}
	return f
}

func egressLevel(e dist.DiagEgress) string {
	if e.OK {
		return "info"
	}
	return "warn"
}

func egressMessage(what string, e dist.DiagEgress) string {
	if !e.OK {
		return what + "探测失败：" + e.Error
	}
	s := what + " " + e.IP
	if e.Country != "" {
		s += "（" + e.Country + "）"
	}
	return s
}

func diagNet(s trust.Status) dist.DiagNet {
	n := s.Network
	return dist.DiagNet{Interface: n.Interface, WiFi: n.WiFi, Trusted: s.Trusted, SSIDReadable: n.SSID != "", Hint: n.Hint}
}

func netName(n dist.DiagNet) string {
	switch {
	case n.Interface == "":
		return "无网络"
	case n.WiFi && n.Trusted:
		return "受信 Wi-Fi（" + n.Interface + "）"
	case n.WiFi && !n.SSIDReadable:
		return "Wi-Fi，读不到名称（" + n.Interface + "）"
	case n.WiFi:
		return "Wi-Fi（" + n.Interface + "）"
	default:
		return n.Interface
	}
}

func netFields(n dist.DiagNet) map[string]string {
	f := map[string]string{
		"interface": n.Interface, "wifi": strconv.FormatBool(n.WiFi),
		"trusted": strconv.FormatBool(n.Trusted), "ssid_readable": strconv.FormatBool(n.SSIDReadable),
	}
	if n.Hint != "" {
		f["hint"] = n.Hint
	}
	return f
}

func stateLevel(state string) string {
	switch state {
	case killswitch.StateHeld, killswitch.StateBlocking, killswitch.StateProxyOnly:
		return "warn"
	case killswitch.StateError:
		return "error"
	}
	return "info"
}

func stateName(state string) string {
	switch state {
	case killswitch.StateOff:
		return "未启用"
	case killswitch.StateProtecting:
		return "保护中"
	case killswitch.StateProxyOnly:
		return "TUN 未接管"
	case killswitch.StateBlocking:
		return "内核不可用，已断开"
	case killswitch.StateError:
		return "规则出错"
	case killswitch.StateDirect:
		return "受信网络直连"
	case killswitch.StateHeld:
		return "已阻断"
	}
	return state
}

var wifiNameRE = regexp.MustCompile(`Wi-Fi「[^」]*」`)

// redact 去掉原因文字里的 Wi-Fi 名称。
func redact(s string) string { return wifiNameRE.ReplaceAllString(s, "Wi-Fi「…」") }

func diagRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, d *diagnostics, svc panel.Services) {
	mux.Handle("GET /api/v1/desktop/diagnostics", authed(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, d.Status())
	}))
	mux.Handle("PUT /api/v1/desktop/diagnostics", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, apiErr{"bad_request", "请求内容无效"})
			return
		}
		st, err := loadSettings(r.Context(), svc.DB)
		if err == nil {
			st.Diagnostics = in.Enabled
			err = saveSettings(r.Context(), svc.DB, st)
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, apiErr{"failed", err.Error()})
			return
		}
		d.SetEnabled(in.Enabled)
		writeJSON(w, http.StatusOK, d.Status())
	}))
	mux.Handle("POST /api/v1/desktop/diagnostics/upload", authed(func(w http.ResponseWriter, r *http.Request) {
		if err := d.Upload(r.Context()); err != nil {
			writeJSON(w, http.StatusBadGateway, apiErr{"failed", err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, d.Status())
	}))
}
