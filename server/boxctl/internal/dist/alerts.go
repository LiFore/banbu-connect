package dist

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // 按日、按月统计用配置的时区，服务器上可能没有时区数据

	"boxctl/internal/notify"
	"boxctl/internal/subscriptions"
)

// 用量提醒（S1.12）：每 5 分钟检查一次——
//   - 节点流量：经过某个节点或代理组的代理流量（遥测上报，路径上每一跳都算）按自然日 / 自然月累计，超过上限时提醒；
//   - 订阅：机场返回的已用流量占比、剩余流量、到期时间；
//   - AI 账号：客户端上报的额度（Claude / Codex 各窗口已用百分比，Gemini 剩余比例）。
// 同一件事只提醒一次（alert_state），条件恢复后或进入新的统计周期 / 额度窗口后才会再提醒。
// 订阅自动更新（管理服务定时拉取）后节点有变化时自动发布，并推送变化；失败时推送原因。

const (
	alertRulesKey = "alert_rules"
	alertEvery    = 5 * time.Minute
	quotaStale    = 24 * time.Hour // 超过这么久没更新的额度不再判断
	gb            = 1 << 30
)

// TrafficRule 是一个节点 / 代理组的流量上限。
type TrafficRule struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`   // 节点或代理组的名称（与客户端内核里的名称一致）
	Period  string  `json:"period"` // day / month
	LimitGB float64 `json:"limit_gb"`
	Enabled bool    `json:"enabled"`
}

// AlertRules 是提醒规则。数值为 0 表示不提醒这一项。
type AlertRules struct {
	Timezone       string        `json:"timezone"`
	Traffic        []TrafficRule `json:"traffic"`
	SubUsedPercent float64       `json:"sub_used_percent"`
	SubRemainGB    float64       `json:"sub_remain_gb"`
	SubExpireDays  int           `json:"sub_expire_days"`
	AIUsedPercent  float64       `json:"ai_used_percent"`
	AIExpireDays   int           `json:"ai_expire_days"` // AI 账号离订阅到期不到几天时提醒（到期日由管理员填写）
	SubUpdate      bool          `json:"sub_update"`     // 推送订阅自动更新的结果
}

func defaultAlertRules() AlertRules {
	return AlertRules{Timezone: "Asia/Shanghai", Traffic: []TrafficRule{}, SubUsedPercent: 90, SubExpireDays: 3, AIUsedPercent: 90, AIExpireDays: 3, SubUpdate: true}
}

// AlertRules 返回提醒规则（没有保存过时是默认值）。
func (s *Server) AlertRules(ctx context.Context) (AlertRules, error) {
	r := defaultAlertRules()
	if err := s.loadSetting(ctx, alertRulesKey, &r); err != nil {
		return r, err
	}
	if r.Traffic == nil {
		r.Traffic = []TrafficRule{}
	}
	return r, nil
}

// SaveAlertRules 校验并保存提醒规则。
func (s *Server) SaveAlertRules(ctx context.Context, r AlertRules) (AlertRules, error) {
	r.Timezone = strings.TrimSpace(r.Timezone)
	if r.Timezone == "" {
		r.Timezone = "Asia/Shanghai"
	}
	if _, err := time.LoadLocation(r.Timezone); err != nil {
		return r, &InputError{"时区无效，如 Asia/Shanghai"}
	}
	if r.SubUsedPercent < 0 || r.SubUsedPercent > 100 || r.AIUsedPercent < 0 || r.AIUsedPercent > 100 {
		return r, &InputError{"百分比应在 0～100 之间"}
	}
	if r.SubRemainGB < 0 || r.SubExpireDays < 0 || r.AIExpireDays < 0 {
		return r, &InputError{"阈值不能为负数"}
	}
	clean := []TrafficRule{}
	for _, t := range r.Traffic {
		t.Name = strings.TrimSpace(t.Name)
		if t.Name == "" {
			return r, &InputError{"流量规则要选择节点或代理组"}
		}
		if t.Period != "day" && t.Period != "month" {
			return r, &InputError{"统计周期应为按日或按月"}
		}
		if t.LimitGB <= 0 {
			return r, &InputError{fmt.Sprintf("「%s」的流量上限应大于 0", t.Name)}
		}
		if t.ID == "" {
			b := make([]byte, 6)
			_, _ = rand.Read(b)
			t.ID = hex.EncodeToString(b)
		}
		clean = append(clean, t)
	}
	r.Traffic = clean
	return r, s.saveSetting(ctx, alertRulesKey, r)
}

// ---- 去重 ----

// fire 记下 key 并返回 true；已经提醒过时返回 false。
func (s *Server) fire(ctx context.Context, key string) bool {
	res, err := s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO alert_state (key, fired_at) VALUES (?, ?)`, key, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// rearm 删除以 prefix 开头的记录：条件已恢复，下次超过时重新提醒。
func (s *Server) rearm(ctx context.Context, prefix string) {
	_, _ = s.DB.ExecContext(ctx, `DELETE FROM alert_state WHERE substr(key, 1, ?) = ?`, len(prefix), prefix)
}

// ---- 检查 ----

// periodStart 是 t 所在自然日 / 自然月在 loc 时区的开始时间，以及周期的名称。
func periodStart(t time.Time, period string, loc *time.Location) (time.Time, string) {
	lt := t.In(loc)
	if period == "month" {
		return time.Date(lt.Year(), lt.Month(), 1, 0, 0, 0, 0, loc), lt.Format("2006-01")
	}
	return time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc), lt.Format("2006-01-02")
}

// TrafficUsage 是一个节点 / 代理组从 since 起的代理流量（字节）。
func (s *Server) TrafficUsage(ctx context.Context, name string, since time.Time) (up, down int64, err error) {
	// 小时汇总按 UTC 整点；时区不是整点偏移时，周期开始那一小时整体计入。
	from := since.UTC().Truncate(time.Hour).Format(time.RFC3339)
	err = s.DB.QueryRowContext(ctx, `SELECT COALESCE(SUM(upload), 0), COALESCE(SUM(download), 0) FROM traffic_hours WHERE name = ? AND hour >= ?`,
		name, from).Scan(&up, &down)
	return up, down, err
}

func fmtGB(b int64) string {
	g := float64(b) / gb
	if g >= 100 {
		return fmt.Sprintf("%.0f GB", g)
	}
	return fmt.Sprintf("%.2f GB", g)
}

var periodNames = map[string]string{"day": "今天", "month": "本月"}

// CheckAlerts 检查一遍所有提醒规则，返回发出的提醒数。
func (s *Server) CheckAlerts(ctx context.Context) (int, error) {
	r, err := s.AlertRules(ctx)
	if err != nil {
		return 0, err
	}
	loc, err := time.LoadLocation(r.Timezone)
	if err != nil {
		loc = time.UTC
	}
	now := time.Now()
	sent := 0
	send := func(key, kind string, m notify.Message) {
		if s.fire(ctx, key) {
			s.Send(ctx, kind, m)
			sent++
		}
	}

	for _, t := range r.Traffic {
		if !t.Enabled {
			continue
		}
		start, label := periodStart(now, t.Period, loc)
		up, down, err := s.TrafficUsage(ctx, t.Name, start)
		if err != nil {
			return sent, err
		}
		limit := int64(t.LimitGB * gb)
		if up+down >= limit {
			send(fmt.Sprintf("traffic:%s:%s:%g", t.ID, label, t.LimitGB), "node_traffic", notify.Message{
				Title:  fmt.Sprintf("节点流量超限：%s", t.Name),
				Body:   fmt.Sprintf("%s已用 %s，上限 %g GB（上行 %s，下行 %s）。只统计经过代理的流量。", periodNames[t.Period], fmtGB(up+down), t.LimitGB, fmtGB(up), fmtGB(down)),
				Urgent: true,
			})
		}
	}

	if s.Subs != nil {
		subs, err := s.Subs.List(ctx)
		if err != nil {
			return sent, err
		}
		for _, sb := range subs {
			ui := sb.UserInfo
			if !sb.Enabled || ui == nil {
				continue
			}
			name := sb.DisplayName
			if name == "" {
				name = sb.Name
			}
			used := ui.Upload + ui.Download
			if ui.Total > 0 {
				pct := float64(used) / float64(ui.Total) * 100
				key := "sub-used:" + sb.ID
				if r.SubUsedPercent > 0 && pct >= r.SubUsedPercent {
					send(key, "sub_usage", notify.Message{
						Title:  fmt.Sprintf("订阅流量已用 %.0f%%：%s", math.Floor(pct), name),
						Body:   fmt.Sprintf("已用 %s / 共 %s，剩余 %s。", fmtGB(used), fmtGB(ui.Total), fmtGB(max(ui.Total-used, 0))),
						Urgent: pct >= 100,
					})
				} else {
					s.rearm(ctx, key) // 重置流量或换了套餐后，下次再提醒
				}
				rkey := "sub-remain:" + sb.ID
				if r.SubRemainGB > 0 && float64(ui.Total-used) <= r.SubRemainGB*gb {
					send(rkey, "sub_usage", notify.Message{
						Title: fmt.Sprintf("订阅剩余流量不足 %g GB：%s", r.SubRemainGB, name),
						Body:  fmt.Sprintf("剩余 %s（已用 %s / 共 %s）。", fmtGB(max(ui.Total-used, 0)), fmtGB(used), fmtGB(ui.Total)),
					})
				} else {
					s.rearm(ctx, rkey)
				}
			}
			if ui.Expire > 0 && r.SubExpireDays > 0 {
				exp := time.Unix(ui.Expire, 0)
				if left := exp.Sub(now); left <= time.Duration(r.SubExpireDays)*24*time.Hour {
					title := fmt.Sprintf("订阅即将到期：%s", name)
					if left <= 0 {
						title = fmt.Sprintf("订阅已到期：%s", name)
					}
					send(fmt.Sprintf("sub-expire:%s:%d", sb.ID, ui.Expire), "sub_expire", notify.Message{
						Title:  title,
						Body:   fmt.Sprintf("到期时间 %s。", exp.In(loc).Format("2006-01-02 15:04")),
						Urgent: left <= 24*time.Hour,
					})
				}
			}
		}
	}

	if r.AIUsedPercent > 0 {
		if err := s.checkAIQuota(ctx, r.AIUsedPercent, now, send); err != nil {
			return sent, err
		}
	}
	if r.AIExpireDays > 0 {
		if err := s.checkAIExpiry(ctx, r.AIExpireDays, now, loc, send); err != nil {
			return sent, err
		}
	}
	return sent, nil
}

// checkAIQuota 检查 AI 账号的额度。
func (s *Server) checkAIQuota(ctx context.Context, threshold float64, now time.Time, send func(key, kind string, m notify.Message)) error {
	accounts, err := s.ClaudeAccounts(ctx)
	if err != nil {
		return err
	}
	kindNames := map[string]string{KindClaude: "Claude", KindCodex: "Codex"}
	for _, a := range accounts {
		q := a.Quota
		if q == nil || q.Error != "" || now.Sub(q.CheckedAt) > quotaStale {
			continue
		}
		for _, w := range q.Windows {
			prefix := fmt.Sprintf("ai:%s:%d:%s:", a.Kind, a.ID, w.Key)
			if w.UsedPercent < threshold {
				s.rearm(ctx, prefix)
				continue
			}
			window := w.ResetAt
			if window == "" {
				window = now.UTC().Format("2006-01-02")
			}
			body := fmt.Sprintf("%s 已用 %.0f%%。", w.Label, w.UsedPercent)
			if t, err := time.Parse(time.RFC3339, w.ResetAt); err == nil {
				body += fmt.Sprintf("将于 %s 重置。", t.Local().Format("01-02 15:04"))
			}
			send(prefix+window, "ai_quota", notify.Message{
				Title:  fmt.Sprintf("%s 额度已用 %.0f%%：%s", kindNames[a.Kind], w.UsedPercent, a.Email),
				Body:   body,
				Urgent: w.UsedPercent >= 100,
			})
		}
	}

	// Gemini：每个账号每一项最近一次采样的剩余比例。
	rows, err := s.DB.QueryContext(ctx, `SELECT q.account_id, a.email, q.bucket, q.label, q.remaining, q.reset_at, MAX(q.sampled_at)
		FROM gemini_quota q JOIN gemini_accounts a ON a.id = q.account_id GROUP BY q.account_id, q.bucket`)
	if err != nil {
		return err
	}
	type bucket struct {
		id                                  int64
		email, key, label, resetAt, sampled string
		remaining                           float64
	}
	list := []bucket{}
	for rows.Next() {
		var b bucket
		if err := rows.Scan(&b.id, &b.email, &b.key, &b.label, &b.remaining, &b.resetAt, &b.sampled); err != nil {
			rows.Close()
			return err
		}
		list = append(list, b)
	}
	rows.Close()
	for _, b := range list {
		if t, err := time.Parse(time.RFC3339, b.sampled); err == nil && now.Sub(t) > quotaStale {
			continue
		}
		used := (1 - b.remaining) * 100
		prefix := fmt.Sprintf("gemini:%d:%s:", b.id, b.key)
		if used < threshold {
			s.rearm(ctx, prefix)
			continue
		}
		window := b.resetAt
		if window == "" {
			window = now.UTC().Format("2006-01-02")
		}
		label := b.label
		if label == "" {
			label = b.key
		}
		send(prefix+window, "ai_quota", notify.Message{
			Title: fmt.Sprintf("Gemini 额度已用 %.0f%%：%s", used, b.email),
			Body:  fmt.Sprintf("%s 剩余 %.0f%%。", label, b.remaining*100),
		})
	}
	return nil
}

// RunAlerts 定时检查提醒规则，ctx 结束时返回。
func (s *Server) RunAlerts(ctx context.Context, log *slog.Logger) {
	timer := time.NewTimer(time.Minute) // 启动后先等客户端上报
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if _, err := s.CheckAlerts(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Warn("check alerts failed", "err", err)
		}
		timer.Reset(alertEvery)
	}
}

// ---- 订阅自动更新 ----

// subUpdates 收集一轮定时更新里各订阅的结果，结束后统一发布、推送。
type subUpdates struct {
	mu      sync.Mutex
	changed []string // 每个订阅一行摘要
}

func diffNames(list []subscriptions.NodeRef) string {
	names := []string{}
	for i, n := range list {
		if i == 5 {
			names = append(names, fmt.Sprintf("等 %d 个", len(list)))
			break
		}
		names = append(names, n.DisplayName)
	}
	return strings.Join(names, "、")
}

// OnSubscriptionUpdate 是订阅定时更新的回调：失败时推送原因（同一原因只推一次），成功且有变化时记下摘要。
func (s *Server) OnSubscriptionUpdate(sb subscriptions.Subscription, d subscriptions.Diff, err error) {
	ctx := context.Background()
	name := sb.DisplayName
	if name == "" {
		name = sb.Name
	}
	prefix := "sub-fail:" + sb.ID + ":"
	if err != nil {
		r, _ := s.AlertRules(ctx)
		if !r.SubUpdate {
			return
		}
		title := "订阅自动更新失败：" + name
		if errors.Is(err, subscriptions.ErrNeedsConfirm) {
			title = "订阅需要手动确认更新：" + name
		}
		if s.fire(ctx, prefix+clipConn(err.Error())) {
			s.Send(ctx, "sub_update", notify.Message{Title: title, Body: err.Error()})
		}
		return
	}
	s.rearm(ctx, prefix)
	if len(d.Added)+len(d.Removed)+len(d.Changed) == 0 {
		return
	}
	parts := []string{}
	if len(d.Added) > 0 {
		parts = append(parts, fmt.Sprintf("新增 %d（%s）", len(d.Added), diffNames(d.Added)))
	}
	if len(d.Removed) > 0 {
		parts = append(parts, fmt.Sprintf("删除 %d（%s）", len(d.Removed), diffNames(d.Removed)))
	}
	if len(d.Changed) > 0 {
		parts = append(parts, fmt.Sprintf("修改 %d", len(d.Changed)))
	}
	s.subUpdates.mu.Lock()
	s.subUpdates.changed = append(s.subUpdates.changed, fmt.Sprintf("%s：%s", name, strings.Join(parts, "，")))
	s.subUpdates.mu.Unlock()
}

// AfterSubscriptionsUpdated 在一轮定时更新有节点变化后调用：自动发布新版本，推送变化摘要。
func (s *Server) AfterSubscriptionsUpdated() {
	ctx := context.Background()
	s.subUpdates.mu.Lock()
	lines := s.subUpdates.changed
	s.subUpdates.changed = nil
	s.subUpdates.mu.Unlock()
	if len(lines) == 0 {
		return
	}
	r, _ := s.AlertRules(ctx)
	if changed, err := s.draftChanged(ctx); err == nil && !changed {
		// 变化的节点不在任何下发范围里（没被代理组使用），已发布的版本不用变。
		if r.SubUpdate {
			s.Send(ctx, "sub_update", notify.Message{Title: "订阅已更新，下发内容没有变化", Body: strings.Join(lines, "\n")})
		}
		return
	}
	p, err := s.Publish(ctx, "订阅自动更新")
	if err != nil {
		if r.SubUpdate {
			s.Send(ctx, "sub_update", notify.Message{Title: "订阅已更新，但自动发布失败", Body: err.Error() + "\n" + strings.Join(lines, "\n"), Urgent: true})
		}
		return
	}
	if r.SubUpdate {
		s.Send(ctx, "sub_update", notify.Message{Title: fmt.Sprintf("订阅已更新并发布 v%d", p.Version), Body: strings.Join(lines, "\n")})
	}
}

// draftChanged 判断草稿和已发布的版本是否不同（与管理页面「发布」页的判断相同）。
func (s *Server) draftChanged(ctx context.Context) (bool, error) {
	p, err := s.Draft(ctx)
	if err != nil {
		return false, err
	}
	cur, ok, err := s.Current(ctx)
	if err != nil || !ok {
		return true, err
	}
	cur.Version, cur.PublishedAt, cur.Note = 0, time.Time{}, ""
	a, _ := json.Marshal(cur)
	b, _ := json.Marshal(p)
	return string(a) != string(b), nil
}

// ---- 接口 ----

// TrafficTarget 是可以设流量上限的节点或代理组。
type TrafficTarget struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"` // node / group
	Day   int64  `json:"day"`  // 今天的代理流量（字节）
	Month int64  `json:"month"`
}

func (s *Server) trafficTargets(ctx context.Context) ([]TrafficTarget, error) {
	r, err := s.AlertRules(ctx)
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(r.Timezone)
	if err != nil {
		loc = time.UTC
	}
	out := []TrafficTarget{}
	seen := map[string]bool{}
	add := func(name, kind string) error {
		if name == "" || seen[name] {
			return nil
		}
		seen[name] = true
		day, _ := periodStart(time.Now(), "day", loc)
		month, _ := periodStart(time.Now(), "month", loc)
		du, dd, err := s.TrafficUsage(ctx, name, day)
		if err != nil {
			return err
		}
		mu, md, err := s.TrafficUsage(ctx, name, month)
		if err != nil {
			return err
		}
		out = append(out, TrafficTarget{Name: name, Kind: kind, Day: du + dd, Month: mu + md})
		return nil
	}
	ns, err := s.Nodes.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range ns {
		if err := add(n.DisplayName, "node"); err != nil {
			return nil, err
		}
	}
	gs, err := s.Groups.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, g := range gs {
		if err := add(g.Name, "group"); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Server) alertAdminRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, log *slog.Logger) {
	fail := func(w http.ResponseWriter, err error) { writeErr(w, err, log) }
	mux.Handle("GET /api/v1/dist/alerts", authed(func(w http.ResponseWriter, r *http.Request) {
		rules, err := s.AlertRules(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rules)
	}))
	mux.Handle("PUT /api/v1/dist/alerts", authed(func(w http.ResponseWriter, r *http.Request) {
		var in AlertRules
		if !decode(w, r, &in) {
			return
		}
		rules, err := s.SaveAlertRules(r.Context(), in)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rules)
	}))
	mux.Handle("GET /api/v1/dist/alerts/targets", authed(func(w http.ResponseWriter, r *http.Request) {
		list, err := s.trafficTargets(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}))
	// 立即检查一次（保存规则后验证）。
	mux.Handle("POST /api/v1/dist/alerts/check", authed(func(w http.ResponseWriter, r *http.Request) {
		n, err := s.CheckAlerts(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"sent": n})
	}))
}
