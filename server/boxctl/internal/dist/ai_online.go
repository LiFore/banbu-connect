package dist

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sort"
	"time"
)

// ---- AI 账号在线时长（每台客户端用每个账号的时段，按切换时间算）----
//
// 客户端每分钟（以及在用的账号变化时立即）上报此刻在用的账号，管理服务据此维护时段（ai_online_sessions）：
// 新出现的账号开一段，消失的账号（切走、进程退出）在这次上报时结束，客户端超过 aiOnlineGap 没有上报（关机、断网）
// 时在最后一次上报时结束。统计按时段与查询范围的重叠部分累加。

const (
	// aiOnlineGap 是客户端多久没上报就算离线（客户端每分钟上报一次）。
	aiOnlineGap = 3 * time.Minute
	// aiOnlineKeep 是时段的保留时间。
	aiOnlineKeep = 400 * 24 * time.Hour
)

// AIOnlineAccount 是客户端上报的一个在用账号。
type AIOnlineAccount struct {
	Kind      string `json:"kind"` // claude / codex / gemini
	AccountID int64  `json:"account_id"`
}

func onlineTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseOnlineTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

type onlineKey struct {
	kind string
	id   int64
}

// aiAccountEmail 找账号的邮箱；账号不存在时返回 ErrNotFound。
func aiAccountEmail(ctx context.Context, tx *sql.Tx, kind string, id int64) (string, error) {
	var email string
	var err error
	switch kind {
	case KindClaude, KindCodex:
		err = tx.QueryRowContext(ctx, `SELECT email FROM claude_accounts WHERE id = ? AND kind = ?`, id, kind).Scan(&email)
	case KindGemini:
		err = tx.QueryRowContext(ctx, `SELECT email FROM gemini_accounts WHERE id = ?`, id).Scan(&email)
	default:
		return "", ErrNotFound
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return email, err
}

// RecordAIOnline 记录客户端此刻在用的账号（覆盖式：没报的账号视为不在用）。
func (s *Server) RecordAIOnline(ctx context.Context, deviceID int64, accounts []AIOnlineAccount, now time.Time) error {
	if len(accounts) > 200 {
		return &InputError{"在用账号太多"}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	type open struct {
		id       int64
		lastSeen time.Time
	}
	opens := map[onlineKey]open{}
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, account_id, last_seen FROM ai_online_sessions WHERE device_id = ? AND end_at = ''`, deviceID)
	if err != nil {
		return err
	}
	var stale []int64 // 同一账号重复的未结束时段（不应出现），一并结束
	for rows.Next() {
		var o open
		var k onlineKey
		var seen string
		if err := rows.Scan(&o.id, &k.kind, &k.id, &seen); err != nil {
			rows.Close()
			return err
		}
		o.lastSeen = parseOnlineTime(seen)
		if prev, dup := opens[k]; dup {
			stale = append(stale, prev.id)
		}
		opens[k] = o
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	nowS := onlineTime(now)
	closeAt := func(id int64, at string) error {
		_, err := tx.ExecContext(ctx, `UPDATE ai_online_sessions SET end_at = ? WHERE id = ?`, at, id)
		return err
	}
	for _, id := range stale {
		if err := closeAt(id, nowS); err != nil {
			return err
		}
	}

	want := map[onlineKey]bool{}
	for _, a := range accounts {
		want[onlineKey{a.Kind, a.AccountID}] = true
	}
	// 已有的时段：还在用且没断过的续上；断过（客户端离线过）的在最后一次上报时结束；不再用的在这次上报时结束（切换时间）。
	for k, o := range opens {
		offline := now.Sub(o.lastSeen) > aiOnlineGap
		switch {
		case want[k] && !offline:
			if _, err := tx.ExecContext(ctx, `UPDATE ai_online_sessions SET last_seen = ? WHERE id = ?`, nowS, o.id); err != nil {
				return err
			}
			delete(want, k)
		case offline:
			if err := closeAt(o.id, onlineTime(o.lastSeen)); err != nil {
				return err
			}
		default:
			if err := closeAt(o.id, nowS); err != nil {
				return err
			}
		}
	}
	// 新出现的账号开一段。账号已删除（或类型不对）的跳过。
	for k := range want {
		email, err := aiAccountEmail(ctx, tx, k.kind, k.id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ai_online_sessions (device_id, kind, account_id, email, start_at, last_seen)
			VALUES (?, ?, ?, ?, ?, ?)`, deviceID, k.kind, k.id, email, nowS, nowS); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CloseStaleAIOnline 结束客户端已离线的时段（结束时间是最后一次上报），并删掉过了保留期的时段。
func (s *Server) CloseStaleAIOnline(ctx context.Context, now time.Time) error {
	if _, err := s.DB.ExecContext(ctx, `UPDATE ai_online_sessions SET end_at = last_seen WHERE end_at = '' AND last_seen < ?`,
		onlineTime(now.Add(-aiOnlineGap))); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, `DELETE FROM ai_online_sessions WHERE end_at != '' AND end_at < ?`, onlineTime(now.Add(-aiOnlineKeep)))
	return err
}

// RunAIOnline 每分钟结束离线客户端的时段，直到 ctx 结束。
func (s *Server) RunAIOnline(ctx context.Context, log *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if err := s.CloseStaleAIOnline(ctx, time.Now()); err != nil && !errors.Is(err, context.Canceled) {
			log.Warn("close stale ai online sessions failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---- 查询 ----

// AIOnlineFilter 是统计的筛选条件，零值表示不限。
type AIOnlineFilter struct {
	DeviceID  int64
	Kind      string
	AccountID int64
}

// AIOnlineSession 是一段在线时段。
type AIOnlineSession struct {
	ID         int64  `json:"id"`
	DeviceID   int64  `json:"device_id"`
	DeviceName string `json:"device_name"`
	Kind       string `json:"kind"`
	AccountID  int64  `json:"account_id"`
	Email      string `json:"email"`
	StartAt    string `json:"start_at"`
	EndAt      string `json:"end_at"` // 空表示还在线
	Seconds    int64  `json:"seconds"`
}

type onlineRow struct {
	AIOnlineSession
	start, end time.Time // end 为有效结束时间：进行中的是 now（客户端已离线但还没被清理的是最后一次上报）
}

// onlineSessions 取与 [from, to) 重叠的时段，按开始时间从新到旧。
func (s *Server) onlineSessions(ctx context.Context, from, to, now time.Time, f AIOnlineFilter) ([]onlineRow, error) {
	q := `SELECT o.id, o.device_id, COALESCE(d.name, ''), o.kind, o.account_id, o.email, o.start_at, o.last_seen, o.end_at
		FROM ai_online_sessions o LEFT JOIN devices d ON d.id = o.device_id
		WHERE o.start_at < ? AND (o.end_at = '' OR o.end_at > ?)`
	args := []any{onlineTime(to), onlineTime(from)}
	if f.DeviceID != 0 {
		q += ` AND o.device_id = ?`
		args = append(args, f.DeviceID)
	}
	if f.Kind != "" {
		q += ` AND o.kind = ?`
		args = append(args, f.Kind)
	}
	if f.AccountID != 0 {
		q += ` AND o.account_id = ?`
		args = append(args, f.AccountID)
	}
	rows, err := s.DB.QueryContext(ctx, q+` ORDER BY o.start_at DESC, o.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []onlineRow
	for rows.Next() {
		var r onlineRow
		var seen string
		if err := rows.Scan(&r.ID, &r.DeviceID, &r.DeviceName, &r.Kind, &r.AccountID, &r.Email, &r.StartAt, &seen, &r.EndAt); err != nil {
			return nil, err
		}
		r.start = parseOnlineTime(r.StartAt)
		switch last := parseOnlineTime(seen); {
		case r.EndAt != "":
			r.end = parseOnlineTime(r.EndAt)
		case now.Sub(last) > aiOnlineGap:
			r.end = last
		default:
			r.end = now
		}
		r.Seconds = int64(r.end.Sub(r.start) / time.Second)
		out = append(out, r)
	}
	return out, rows.Err()
}

// overlap 是时段落在 [from, to) 里的秒数。
func (r onlineRow) overlap(from, to time.Time) int64 {
	a, b := r.start, r.end
	if a.Before(from) {
		a = from
	}
	if b.After(to) {
		b = to
	}
	if !b.After(a) {
		return 0
	}
	return int64(b.Sub(a) / time.Second)
}

// live 是这一段还在进行（客户端在线且在用这个账号）。
func (r onlineRow) live(now time.Time) bool { return r.EndAt == "" && !r.end.Before(now) }

// AIOnlineTotal 是一台客户端用一个账号的合计。
type AIOnlineTotal struct {
	DeviceID    int64  `json:"device_id"`
	DeviceName  string `json:"device_name"`
	Kind        string `json:"kind"`
	AccountID   int64  `json:"account_id"`
	Email       string `json:"email"`
	Seconds     int64  `json:"seconds"`
	Sessions    int    `json:"sessions"`     // 范围内的时段数（每次切换进来算一段）
	OnlineSince string `json:"online_since"` // 此刻在线时这一段的开始时间
	LastAt      string `json:"last_at"`      // 最近一次在线的时间（进行中为空）
}

// AIOnlineDay 是一个账号在一天里的合计（所有客户端）。
type AIOnlineDay struct {
	Day       string `json:"day"` // YYYY-MM-DD，按查询给的时区
	Kind      string `json:"kind"`
	AccountID int64  `json:"account_id"`
	Seconds   int64  `json:"seconds"`
}

// AIOnlineStats 是统计页的数据。
type AIOnlineStats struct {
	From     string            `json:"from"`
	To       string            `json:"to"`
	Totals   []AIOnlineTotal   `json:"totals"`
	Days     []AIOnlineDay     `json:"days"`
	Sessions []AIOnlineSession `json:"sessions"` // 最近的若干段（切换记录）
}

const aiOnlineMaxSessions = 500

// AIOnlineStatsFor 统计 [from, to) 内每台客户端每个账号的在线时长，按天的合计按 loc 切分。
func (s *Server) AIOnlineStatsFor(ctx context.Context, from, to time.Time, loc *time.Location, f AIOnlineFilter, now time.Time) (AIOnlineStats, error) {
	if to.After(now) {
		to = now
	}
	out := AIOnlineStats{From: onlineTime(from), To: onlineTime(to), Totals: []AIOnlineTotal{}, Days: []AIOnlineDay{}, Sessions: []AIOnlineSession{}}
	list, err := s.onlineSessions(ctx, from, to, now, f)
	if err != nil {
		return out, err
	}
	type tkey struct {
		device int64
		onlineKey
	}
	totals := map[tkey]*AIOnlineTotal{}
	type dkey struct {
		day string
		onlineKey
	}
	days := map[dkey]int64{}
	for _, r := range list {
		sec := r.overlap(from, to)
		k := tkey{r.DeviceID, onlineKey{r.Kind, r.AccountID}}
		t := totals[k]
		if t == nil {
			t = &AIOnlineTotal{DeviceID: r.DeviceID, DeviceName: r.DeviceName, Kind: r.Kind, AccountID: r.AccountID, Email: r.Email}
			totals[k] = t
		}
		t.Seconds += sec
		t.Sessions++
		if r.live(now) {
			t.OnlineSince = r.StartAt
		} else if e := onlineTime(r.end); e > t.LastAt {
			t.LastAt = e
		}
		// 按天切分（loc 的零点）。
		for d := dayStart(maxTime(r.start, from), loc); d.Before(minTime(r.end, to)); d = d.AddDate(0, 0, 1) {
			if sec := r.overlap(maxTime(d, from), minTime(d.AddDate(0, 0, 1), to)); sec > 0 {
				days[dkey{d.Format("2006-01-02"), onlineKey{r.Kind, r.AccountID}}] += sec
			}
		}
		if len(out.Sessions) < aiOnlineMaxSessions {
			out.Sessions = append(out.Sessions, r.AIOnlineSession)
		}
	}
	for _, t := range totals {
		if t.OnlineSince != "" {
			t.LastAt = ""
		}
		out.Totals = append(out.Totals, *t)
	}
	sort.Slice(out.Totals, func(i, j int) bool {
		a, b := out.Totals[i], out.Totals[j]
		if a.Seconds != b.Seconds {
			return a.Seconds > b.Seconds
		}
		if a.DeviceID != b.DeviceID {
			return a.DeviceID < b.DeviceID
		}
		return a.Kind+a.Email < b.Kind+b.Email
	})
	for k, sec := range days {
		out.Days = append(out.Days, AIOnlineDay{Day: k.day, Kind: k.kind, AccountID: k.id, Seconds: sec})
	}
	sort.Slice(out.Days, func(i, j int) bool {
		a, b := out.Days[i], out.Days[j]
		if a.Day != b.Day {
			return a.Day < b.Day
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.AccountID < b.AccountID
	})
	return out, nil
}

func dayStart(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// AIOnlineDevice 是账号卡片上一台客户端的在线时长。
type AIOnlineDevice struct {
	DeviceID    int64  `json:"device_id"`
	DeviceName  string `json:"device_name"`
	Today       int64  `json:"today"`        // 今天（查询给的零点起）的秒数
	Week        int64  `json:"week"`         // 最近 7 天的秒数
	OnlineSince string `json:"online_since"` // 此刻在线时这一段的开始时间
}

// AIOnlineSummary 是一个账号的在线时长（账号卡片用）。
type AIOnlineSummary struct {
	Kind      string           `json:"kind"`
	AccountID int64            `json:"account_id"`
	Today     int64            `json:"today"`
	Week      int64            `json:"week"`
	Devices   []AIOnlineDevice `json:"devices"`
}

// AIOnlineSummaries 汇总每个账号今天（从 today 起）和最近 7 天的在线时长，按客户端分开。
func (s *Server) AIOnlineSummaries(ctx context.Context, today, now time.Time) ([]AIOnlineSummary, error) {
	weekFrom := now.Add(-7 * 24 * time.Hour)
	from := minTime(today, weekFrom)
	list, err := s.onlineSessions(ctx, from, now, now, AIOnlineFilter{})
	if err != nil {
		return nil, err
	}
	byAcc := map[onlineKey]*AIOnlineSummary{}
	devs := map[onlineKey]map[int64]*AIOnlineDevice{}
	var order []onlineKey
	for _, r := range list {
		k := onlineKey{r.Kind, r.AccountID}
		sum := byAcc[k]
		if sum == nil {
			sum = &AIOnlineSummary{Kind: r.Kind, AccountID: r.AccountID}
			byAcc[k], devs[k] = sum, map[int64]*AIOnlineDevice{}
			order = append(order, k)
		}
		d := devs[k][r.DeviceID]
		if d == nil {
			d = &AIOnlineDevice{DeviceID: r.DeviceID, DeviceName: r.DeviceName}
			devs[k][r.DeviceID] = d
		}
		t, w := r.overlap(today, now), r.overlap(weekFrom, now)
		d.Today += t
		d.Week += w
		sum.Today += t
		sum.Week += w
		if r.live(now) {
			d.OnlineSince = r.StartAt
		}
	}
	out := make([]AIOnlineSummary, 0, len(order))
	for _, k := range order {
		sum := byAcc[k]
		for _, d := range devs[k] {
			sum.Devices = append(sum.Devices, *d)
		}
		sort.Slice(sum.Devices, func(i, j int) bool {
			a, b := sum.Devices[i], sum.Devices[j]
			if (a.OnlineSince != "") != (b.OnlineSince != "") {
				return a.OnlineSince != ""
			}
			return a.Week > b.Week
		})
		out = append(out, *sum)
	}
	return out, nil
}
