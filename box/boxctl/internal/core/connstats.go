package core

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"boxctl/internal/dist"
)

// 代理连接统计（S1.8，ADR-048）：桌面客户端和路由盒共用。
//
// 每秒拿一次内核连接快照（桌面端由 RunConnSampler 采，路由盒和连接页共用同一次采样），与上一次比对：
//   - 只统计实际出站不是 DIRECT/REJECT 的连接（Conn.IsProxied），DIRECT 不统计（Q-021）；
//   - 每条连接的增量计入当前分钟的汇总（完整链路 × 域名 × 规则 × 进程/来源 IP）；
//   - 连接消失时记一条关闭记录（最后一次看到的累计流量与时长）；
//   - 内核总流量的增量减去已归属到连接的增量，记为「未归属」，使每端总量与内核计数一致。
// 分钟结束时把汇总（只留前 N 种组合，其余按链路合并为「(其他)」）和关闭记录打成一批，排队上报，
// 上报成功后出队；离线时队列保存在磁盘上（路由盒放在内存盘），联网后补报。

const (
	connStatsMinuteKeys = 200     // 每分钟最多保留的组合数，其余合并
	connStatsMaxRecords = 2000    // 每分钟最多记录的关闭连接
	connStatsMaxQueue   = 24 * 60 // 最多排队一天
	connStatsBatchSend  = 30      // 一次上报的批数
	connStatsTick       = 15 * time.Second
	connStatsUploadGap  = time.Minute
	connStatsOther      = "(其他)"
	connSamplePeriod    = time.Second
	connLivePush        = 3 * time.Second
)

type seenConn struct {
	up, down int64
	conn     Conn
}

type minuteKey struct {
	path, host, rule, payload, process, source string
}

// ConnStats 是代理连接统计的采集器和上报队列。
type ConnStats struct {
	path     string
	kind     string // desktop / box
	client   *dist.Client
	log      *slog.Logger
	decorate func(*dist.ConnBatch) // 补上设备信息（路由盒）或本机局域网地址（桌面端）
	now      func() time.Time
	kick     chan struct{}

	mu        sync.Mutex
	enabled   bool
	primed    bool
	seen      map[string]seenConn
	lastUp    int64
	lastDown  int64
	minute    time.Time
	cur       map[minuteKey]*dist.ConnMinute
	unUp      int64
	unDown    int64
	records   []dist.ConnRecord
	dropped   int64
	queue     []dist.ConnBatch
	lastSeq   int64
	lastSent  time.Time
	lastOK    time.Time
	lastErr   string
	liveUntil time.Time // 管理页面在看实时连接（BXC-F0-030）
	live      []Conn    // 最近一次快照里的代理连接，实时查看用

	upMu sync.Mutex // 串行化上报
}

type connQueueFile struct {
	LastSeq int64            `json:"last_seq"`
	Queue   []dist.ConnBatch `json:"queue"`
}

// NewConnStats 创建采集器。path 为空时队列只在内存里。
func NewConnStats(path, kind string, client *dist.Client, enabled bool, log *slog.Logger) *ConnStats {
	s := &ConnStats{path: path, kind: kind, client: client, log: log, enabled: enabled, now: time.Now,
		kick: make(chan struct{}, 1), seen: map[string]seenConn{}, cur: map[minuteKey]*dist.ConnMinute{}}
	if path != "" {
		var q connQueueFile
		if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &q) == nil {
			s.queue, s.lastSeq = q.Queue, q.LastSeq
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) && log != nil {
			log.Warn("读取代理连接统计队列失败", "err", err)
		}
	}
	if s.lastSeq == 0 {
		s.lastSeq = time.Now().UnixNano() // 换了数据目录也不会和以前的序号冲突
	}
	return s
}

// SetDecorate 设置打包时补充信息的函数。
func (s *ConnStats) SetDecorate(f func(*dist.ConnBatch)) { s.decorate = f }

// Enabled 表示在统计和上报。
func (s *ConnStats) Enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled
}

// SetEnabled 打开或关闭统计。关闭时清空还没上报的数据。
func (s *ConnStats) SetEnabled(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = on
	if !on {
		s.resetLocked()
		s.queue, s.records, s.dropped = nil, nil, 0
		s.cur = map[minuteKey]*dist.ConnMinute{}
		s.unUp, s.unDown = 0, 0
		s.saveLocked()
	}
}

func (s *ConnStats) resetLocked() {
	s.primed, s.seen, s.live = false, map[string]seenConn{}, nil
}

// Observe 处理一次快照。
func (s *ConnStats) Observe(snap ConnSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled {
		return
	}
	m := snap.Time.UTC().Truncate(time.Minute)
	if !s.minute.IsZero() && m.After(s.minute) {
		s.sealLocked()
	}
	if s.minute.IsZero() || m.After(s.minute) {
		s.minute = m
	}
	if !snap.Running {
		s.resetLocked() // 内核停了：计数器会归零，连接全部断开
		return
	}
	if !s.primed {
		// 第一次只记下现状：启动前的流量不算进这一分钟。
		for _, c := range snap.Conns {
			s.seen[c.ID] = seenConn{up: c.Upload, down: c.Download, conn: c}
		}
		s.lastUp, s.lastDown, s.primed = snap.UploadTotal, snap.DownloadTotal, true
		return
	}
	var attrUp, attrDown int64
	present := make(map[string]bool, len(snap.Conns))
	live := make([]Conn, 0)
	for _, c := range snap.Conns {
		present[c.ID] = true
		old, known := s.seen[c.ID]
		du, dd := max(c.Upload-old.up, 0), max(c.Download-old.down, 0)
		attrUp += du
		attrDown += dd
		if c.IsProxied() {
			s.addLocked(c, du, dd, !known)
			live = append(live, c)
		}
		s.seen[c.ID] = seenConn{up: c.Upload, down: c.Download, conn: c}
	}
	for id, old := range s.seen {
		if present[id] {
			continue
		}
		delete(s.seen, id)
		if old.conn.IsProxied() {
			s.recordLocked(old, snap.Time)
		}
	}
	// 内核重启后计数器从 0 开始，差值为负：这一次不计未归属。
	if du := snap.UploadTotal - s.lastUp; du > attrUp {
		s.unUp += du - attrUp
	}
	if dd := snap.DownloadTotal - s.lastDown; dd > attrDown {
		s.unDown += dd - attrDown
	}
	s.lastUp, s.lastDown, s.live = snap.UploadTotal, snap.DownloadTotal, live
}

func hostOf(c Conn) string {
	if c.Host != "" {
		return c.Host
	}
	if h, _, err := net.SplitHostPort(c.Destination); err == nil {
		return h
	}
	return c.Destination
}

func (s *ConnStats) addLocked(c Conn, du, dd int64, isNew bool) {
	path := c.Path()
	k := minuteKey{path: strings.Join(path, "\x1f"), host: hostOf(c), rule: c.Rule, payload: c.RulePayload, process: c.Process, source: c.Source}
	if s.kind == "desktop" {
		k.source = "" // 桌面端的来源都是本机
	}
	a := s.cur[k]
	if a == nil {
		a = &dist.ConnMinute{Minute: s.minute, Path: path, Host: k.host, Rule: c.Rule, Payload: c.RulePayload, Process: c.Process, Source: k.source}
		s.cur[k] = a
	}
	a.Upload += du
	a.Download += dd
	if isNew {
		a.Conns++
	}
}

func (s *ConnStats) recordLocked(old seenConn, end time.Time) {
	if len(s.records) >= connStatsMaxRecords {
		s.dropped++
		return
	}
	c := old.conn
	src := c.Source
	if s.kind == "desktop" {
		src = ""
	}
	s.records = append(s.records, dist.ConnRecord{Start: c.Start.UTC(), End: end.UTC(), Network: c.Network, Host: hostOf(c),
		Destination: c.Destination, Rule: c.Rule, Payload: c.RulePayload, Path: c.Path(), Process: c.Process, Source: src,
		Upload: old.up, Download: old.down})
}

// sealLocked 把当前分钟打成一批放进队列。
func (s *ConnStats) sealLocked() {
	if len(s.cur) == 0 && s.unUp == 0 && s.unDown == 0 && len(s.records) == 0 && s.dropped == 0 {
		return
	}
	b := dist.ConnBatch{Minutes: topMinutes(s.cur), Records: s.records, Dropped: s.dropped}
	if b.Records == nil {
		b.Records = []dist.ConnRecord{}
	}
	if s.unUp > 0 || s.unDown > 0 {
		b.Unattributed = []dist.ConnUnattributed{{Minute: s.minute, Upload: s.unUp, Download: s.unDown}}
	}
	if s.decorate != nil {
		s.decorate(&b)
	}
	s.lastSeq++
	b.Seq = s.lastSeq
	s.queue = append(s.queue, b)
	if len(s.queue) > connStatsMaxQueue {
		s.queue = s.queue[len(s.queue)-connStatsMaxQueue:]
	}
	s.cur, s.records, s.dropped, s.unUp, s.unDown = map[minuteKey]*dist.ConnMinute{}, nil, 0, 0, 0
	s.saveLocked()
}

// topMinutes 按流量留前 N 种组合，其余按（链路, 来源）合并为「(其他)」，链路不丢，拓扑图仍能显示。
func topMinutes(cur map[minuteKey]*dist.ConnMinute) []dist.ConnMinute {
	all := make([]dist.ConnMinute, 0, len(cur))
	for _, a := range cur {
		all = append(all, *a)
	}
	sort.Slice(all, func(i, j int) bool {
		ai, aj := all[i].Upload+all[i].Download, all[j].Upload+all[j].Download
		if ai != aj {
			return ai > aj
		}
		return all[i].Host < all[j].Host
	})
	if len(all) <= connStatsMinuteKeys {
		return all
	}
	out := all[: connStatsMinuteKeys-1 : connStatsMinuteKeys-1]
	merged := map[string]*dist.ConnMinute{}
	var order []string
	for _, a := range all[connStatsMinuteKeys-1:] {
		k := strings.Join(a.Path, "\x1f") + "\x00" + a.Source
		m := merged[k]
		if m == nil {
			m = &dist.ConnMinute{Minute: a.Minute, Path: a.Path, Host: connStatsOther, Source: a.Source}
			merged[k] = m
			order = append(order, k)
		}
		m.Upload += a.Upload
		m.Download += a.Download
		m.Conns += a.Conns
	}
	for _, k := range order {
		out = append(out, *merged[k])
	}
	return out
}

func (s *ConnStats) saveLocked() {
	if s.path == "" {
		return
	}
	b, err := json.Marshal(connQueueFile{LastSeq: s.lastSeq, Queue: s.queue})
	if err == nil {
		tmp := s.path + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, s.path)
		}
	}
	if err != nil && s.log != nil {
		s.log.Warn("保存代理连接统计队列失败", "err", err)
	}
}

// Seal 在分钟已经过去、却一直没有新快照时（内核停了）把数据打包。
func (s *ConnStats) Seal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.now().UTC().Truncate(time.Minute); !s.minute.IsZero() && m.After(s.minute) {
		s.sealLocked()
		s.minute = m
	}
}

// Run 定时打包和上报，直到 ctx 结束。
func (s *ConnStats) Run(ctx context.Context) {
	t := time.NewTicker(connStatsTick)
	defer t.Stop()
	live := time.NewTicker(connLivePush)
	defer live.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-live.C:
			s.pushLive(ctx)
			continue
		case <-t.C:
		case <-s.kick:
		}
		s.Seal()
		s.mu.Lock()
		due := s.enabled && len(s.queue) > 0 && s.now().Sub(s.lastSent) >= connStatsUploadGap
		watching := s.now().Before(s.liveUntil)
		s.mu.Unlock()
		if due {
			if err := s.Upload(ctx); err != nil && s.log != nil {
				s.log.Info("上报代理连接统计失败", "err", err)
			}
		} else if !watching && s.Enabled() && s.bound() {
			// 没有要上报的时候问一下有没有人在看实时连接（BXC-F0-030）。
			if on, err := s.client.ConnLiveWanted(ctx); err == nil && on {
				s.mu.Lock()
				s.liveUntil = s.now().Add(time.Minute)
				s.mu.Unlock()
			}
		}
	}
}

// pushLive 在管理页面看实时连接期间推送当前经代理组的连接（BXC-F0-030）。
func (s *ConnStats) pushLive(ctx context.Context) {
	s.mu.Lock()
	if !s.enabled || !s.now().Before(s.liveUntil) {
		s.mu.Unlock()
		return
	}
	conns := make([]dist.ConnRecord, 0, len(s.live))
	for _, c := range s.live {
		src := c.Source
		if s.kind == "desktop" {
			src = ""
		}
		conns = append(conns, dist.ConnRecord{Start: c.Start.UTC(), Network: c.Network, Host: hostOf(c), Destination: c.Destination,
			Rule: c.Rule, Payload: c.RulePayload, Path: c.Path(), Process: c.Process, Source: src, Upload: c.Upload, Download: c.Download})
	}
	s.mu.Unlock()
	if !s.bound() {
		return
	}
	on, err := s.client.PushConnLive(ctx, conns)
	s.mu.Lock()
	if err == nil && !on {
		s.liveUntil = time.Time{} // 页面关了
	}
	s.mu.Unlock()
}

func (s *ConnStats) bound() bool { return s.client != nil && s.client.Status().Bound }

// Upload 上报排队的批次，成功的出队。
func (s *ConnStats) Upload(ctx context.Context) error {
	if !s.Enabled() || !s.bound() {
		return nil
	}
	s.upMu.Lock()
	defer s.upMu.Unlock()
	for {
		s.mu.Lock()
		s.lastSent = s.now()
		batch := append([]dist.ConnBatch{}, s.queue[:min(len(s.queue), connStatsBatchSend)]...)
		s.mu.Unlock()
		if len(batch) == 0 {
			return nil
		}
		res, err := s.client.ReportConns(ctx, dist.ConnReport{Kind: s.kind, Batches: batch})
		s.mu.Lock()
		if err != nil {
			s.lastErr = err.Error()
			s.mu.Unlock()
			return err
		}
		s.lastErr, s.lastOK = "", s.now()
		if res.Live {
			s.liveUntil = s.now().Add(time.Minute)
		}
		i := 0
		for i < len(s.queue) && s.queue[i].Seq <= res.Acked {
			i++
		}
		s.queue = s.queue[i:]
		s.saveLocked()
		more := len(s.queue) > 0 && i > 0
		s.mu.Unlock()
		if !more {
			return nil
		}
	}
}

// ConnStatsStatus 是「服务端」页上显示的上报状态（BXC-F0-029）。
type ConnStatsStatus struct {
	Enabled    bool            `json:"enabled"`
	Bound      bool            `json:"bound"`
	Pending    int             `json:"pending"` // 排队的批数（约等于分钟数）
	LastUpload *time.Time      `json:"last_upload,omitempty"`
	LastError  string          `json:"last_error"`
	Preview    *dist.ConnBatch `json:"preview,omitempty"` // 最近一批待上报的内容（最多 20 行汇总、20 条记录）
}

// Status 返回上报状态和待上报内容的预览。
func (s *ConnStats) Status() ConnStatsStatus {
	s.mu.Lock()
	st := ConnStatsStatus{Enabled: s.enabled, Pending: len(s.queue), LastError: s.lastErr}
	if !s.lastOK.IsZero() {
		t := s.lastOK
		st.LastUpload = &t
	}
	if n := len(s.queue); n > 0 {
		b := s.queue[n-1]
		b.Minutes = b.Minutes[:min(len(b.Minutes), 20)]
		b.Records = b.Records[:min(len(b.Records), 20)]
		st.Preview = &b
	}
	s.mu.Unlock()
	st.Bound = s.bound()
	return st
}

// RunConnSampler 每秒读一次内核连接交给 stats（桌面端；路由盒由连接页的采样顺带交给它）。
func RunConnSampler(ctx context.Context, sup *Supervisor, stats *ConnStats) {
	t := time.NewTicker(connSamplePeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !stats.Enabled() {
				continue
			}
			raw, running := sup.FetchConnections(ctx)
			stats.Observe(raw.Snapshot(time.Now(), running))
		}
	}
}
