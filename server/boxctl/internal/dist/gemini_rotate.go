package dist

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// ---- 按额度轮换与调度建议（BXC-F0-023）----
//
// 参考 clauth 的 burn-aware 思路，简化为：
// - 每个账号看「最紧的一项」：最近一小时内各项最新采样里剩余比例最低的那项。新版客户端报的是时间窗口
//   （Claude / Gemini 的 5 小时、每周），旧版报的是逐模型额度；同一账号两种都有时（新旧客户端混用、刚升级）只看窗口。
// - 消耗速率：这一项在同一个重置周期里、最近两小时的首末两次采样之差 / 时间；据此估计耗尽时间，晚于重置时间则不会耗尽。
// - 建议顺序：可用的（剩余高于阈值）按重置时间从早到晚（先用快要恢复的，免得额度浪费），同时间剩余多的在前；
//   没有数据的其次；已耗尽的最后，按恢复时间排。
// - 切换建议：客户端当前账号已耗尽或预计 30 分钟内耗尽时，建议换成顺序里第一个可用的账号。
// 客户端额度将尽时按这个顺序自动切换（autoRotateGemini），管理页面也可以下发切换指令。

const (
	// GeminiExhausted 是剩余比例低于它就当作耗尽的阈值。
	GeminiExhausted  = 0.05
	geminiFreshness  = time.Hour
	geminiBurnWindow = 2 * time.Hour
	geminiSoon       = 30 * time.Minute
)

// GeminiUsage 是一个账号的额度汇总。
type GeminiUsage struct {
	AccountID   int64               `json:"account_id"`
	Buckets     []GeminiQuotaBucket `json:"buckets"`
	Known       bool                `json:"known"`     // 最近一小时有采样
	Min         float64             `json:"min"`       // 最紧一项的剩余比例
	MinLabel    string              `json:"min_label"` // 最紧的一项
	minKey      string              // 最紧一项的 key：消耗速率按它找同一项的历史采样
	ResetAt     string              `json:"reset_at"`      // 最紧一项的恢复时间
	BurnPerHour float64             `json:"burn_per_hour"` // 最紧一项每小时消耗的比例；0 表示没在用或不知道
	ExhaustAt   string              `json:"exhaust_at"`    // 预计耗尽时间；恢复前不会耗尽时为空
	SampledAt   string              `json:"sampled_at"`
}

// Exhausted 是已经耗尽（低于阈值）。
func (u GeminiUsage) Exhausted() bool { return u.Known && u.Min <= GeminiExhausted }

// soon 是预计 30 分钟内耗尽。
func (u GeminiUsage) soon(now time.Time) bool {
	t, err := time.Parse(time.RFC3339, u.ExhaustAt)
	return err == nil && t.Before(now.Add(geminiSoon))
}

type quotaRow struct {
	bucket, label, reset string
	remaining            float64
	at                   time.Time
}

// geminiUsage 汇总每个账号的额度。
func (s *Server) geminiUsage(ctx context.Context, now time.Time) (map[int64]GeminiUsage, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT account_id, bucket, label, remaining, reset_at, sampled_at FROM gemini_quota
		WHERE sampled_at >= ? ORDER BY account_id, sampled_at`, now.Add(-geminiBurnWindow).UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byAccount := map[int64][]quotaRow{}
	for rows.Next() {
		var id int64
		var r quotaRow
		var at string
		if err := rows.Scan(&id, &r.bucket, &r.label, &r.remaining, &r.reset, &at); err != nil {
			return nil, err
		}
		r.at, _ = time.Parse(time.RFC3339, at)
		byAccount[id] = append(byAccount[id], r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[int64]GeminiUsage{}
	for id, rs := range byAccount {
		out[id] = summarizeUsage(id, rs, now)
	}
	return out, nil
}

// summarizeUsage 按时间先后的采样算出汇总。
func summarizeUsage(id int64, rs []quotaRow, now time.Time) GeminiUsage {
	u := GeminiUsage{AccountID: id, Buckets: []GeminiQuotaBucket{}, Min: -1}
	latest := map[string]quotaRow{}
	for _, r := range rs {
		latest[r.bucket] = r // 按时间升序，后面的覆盖前面的
	}
	windows := false
	for _, r := range latest {
		if now.Sub(r.at) <= geminiFreshness && isQuotaWindow(r.bucket) {
			windows = true
		}
	}
	var last time.Time
	for _, r := range latest {
		if now.Sub(r.at) > geminiFreshness || (windows && !isQuotaWindow(r.bucket)) {
			continue
		}
		u.Buckets = append(u.Buckets, GeminiQuotaBucket{Key: r.bucket, Label: r.label, Remaining: r.remaining, ResetAt: r.reset})
		if r.at.After(last) {
			last = r.at
		}
		if u.Min < 0 || r.remaining < u.Min || (r.remaining == u.Min && quotaRank(r.bucket) < quotaRank(u.minKey)) {
			u.Min, u.MinLabel, u.ResetAt, u.minKey = r.remaining, r.label, r.reset, r.bucket
		}
	}
	sort.Slice(u.Buckets, func(i, j int) bool {
		a, b := u.Buckets[i], u.Buckets[j]
		if ra, rb := quotaRank(a.Key), quotaRank(b.Key); ra != rb {
			return ra < rb
		}
		return a.Key < b.Key
	})
	if len(u.Buckets) == 0 {
		u.Min = 0
		return u
	}
	u.Known, u.SampledAt = true, last.UTC().Format(time.RFC3339)

	// 最紧一项在当前重置周期里的消耗速率。
	var first, end *quotaRow
	for i := range rs {
		r := &rs[i]
		if r.bucket != u.minKey || r.reset != u.ResetAt {
			continue
		}
		if first == nil {
			first = r
		}
		end = r
	}
	if first != nil && end != nil {
		hours := end.at.Sub(first.at).Hours()
		if hours >= 10.0/60 && first.remaining > end.remaining {
			u.BurnPerHour = (first.remaining - end.remaining) / hours
			exhaust := end.at.Add(time.Duration(end.remaining / u.BurnPerHour * float64(time.Hour)))
			reset, err := time.Parse(time.RFC3339, u.ResetAt)
			if err != nil || exhaust.Before(reset) {
				u.ExhaustAt = exhaust.UTC().Format(time.RFC3339)
			}
		}
	}
	return u
}

// orderAccounts 排出建议的使用顺序（见文件开头）。
func orderAccounts(ids []int64, usage map[int64]GeminiUsage) []int64 {
	rank := func(u GeminiUsage, ok bool) int {
		switch {
		case !ok || !u.Known:
			return 1
		case u.Exhausted():
			return 2
		}
		return 0
	}
	resetKey := func(u GeminiUsage) string {
		if u.ResetAt == "" {
			return "9999" // 不知道何时恢复的排在后面
		}
		return u.ResetAt
	}
	out := append([]int64(nil), ids...)
	sort.SliceStable(out, func(i, j int) bool {
		a, aok := usage[out[i]]
		b, bok := usage[out[j]]
		ra, rb := rank(a, aok), rank(b, bok)
		if ra != rb {
			return ra < rb
		}
		if ra == 1 {
			return false
		}
		if resetKey(a) != resetKey(b) {
			return resetKey(a) < resetKey(b)
		}
		return a.Min > b.Min
	})
	return out
}

// geminiOrder 是同步给客户端的建议顺序，只含有凭据的账号。
func (s *Server) geminiOrder(ctx context.Context, accounts []GeminiSyncAccount) ([]int64, error) {
	var ids []int64
	for _, a := range accounts {
		if len(a.Credential) > 0 {
			ids = append(ids, a.ID)
		}
	}
	if len(ids) == 0 {
		return []int64{}, nil
	}
	usage, err := s.geminiUsage(ctx, time.Now())
	if err != nil {
		return nil, err
	}
	return orderAccounts(ids, usage), nil
}

// GeminiSuggestion 是给一台客户端的切换建议。
type GeminiSuggestion struct {
	AccountID int64  `json:"account_id"`
	Reason    string `json:"reason"`
}

// GeminiDeviceView 是管理页面上一台客户端的情况。
type GeminiDeviceView struct {
	DeviceID      int64             `json:"device_id"`
	Name          string            `json:"name"`
	ActiveID      int64             `json:"active_account_id"`
	ReportedAt    string            `json:"reported_at"`
	Order         []int64           `json:"order"`
	PendingSwitch int64             `json:"pending_switch"` // 已下发、还没执行的切换
	Suggestion    *GeminiSuggestion `json:"suggestion,omitempty"`
}

// GeminiOverview 是管理页面的额度与调度概览。
type GeminiOverview struct {
	Usage     []GeminiUsage      `json:"usage"`
	Devices   []GeminiDeviceView `json:"devices"`
	Threshold float64            `json:"threshold"`
}

// suggest 给出切换建议：当前账号耗尽或快耗尽、而顺序里有别的可用账号时。
func suggest(active int64, order []int64, usage map[int64]GeminiUsage, emails map[int64]string, now time.Time) *GeminiSuggestion {
	cur, known := usage[active]
	reason := ""
	switch {
	case active == 0:
		reason = "还没有使用任何账号"
	case known && cur.Exhausted():
		reason = fmt.Sprintf("%s 的「%s」已用完", emails[active], cur.MinLabel)
	case known && cur.soon(now):
		reason = fmt.Sprintf("%s 的「%s」预计 30 分钟内用完", emails[active], cur.MinLabel)
	default:
		return nil
	}
	for _, id := range order {
		if u, ok := usage[id]; id != active && (!ok || !u.Exhausted()) {
			return &GeminiSuggestion{AccountID: id, Reason: reason + "，建议换成 " + emails[id]}
		}
	}
	return nil
}

// GeminiOverview 汇总各账号额度、各客户端当前账号、建议顺序和切换建议。
func (s *Server) GeminiOverview(ctx context.Context) (GeminiOverview, error) {
	now := time.Now()
	out := GeminiOverview{Usage: []GeminiUsage{}, Devices: []GeminiDeviceView{}, Threshold: GeminiExhausted}
	usage, err := s.geminiUsage(ctx, now)
	if err != nil {
		return out, err
	}
	accounts, err := s.GeminiAccounts(ctx)
	if err != nil {
		return out, err
	}
	emails := map[int64]string{}
	held := map[int64][]int64{} // 客户端 → 分配到的、有凭据的账号
	for _, a := range accounts {
		emails[a.ID] = a.Email
		if u, ok := usage[a.ID]; ok {
			out.Usage = append(out.Usage, u)
		}
		if a.HasCredential {
			for _, d := range a.Devices {
				held[d] = append(held[d], a.ID)
			}
		}
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT d.id, d.name, COALESCE(g.active_account_id, 0), COALESCE(g.reported_at, ''), COALESCE(g.switch_to, 0)
		FROM devices d LEFT JOIN gemini_device_state g ON g.device_id = d.id
		WHERE d.revoked = 0 AND d.id IN (SELECT device_id FROM gemini_effective_assignments) ORDER BY d.name`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var v GeminiDeviceView
		if err := rows.Scan(&v.DeviceID, &v.Name, &v.ActiveID, &v.ReportedAt, &v.PendingSwitch); err != nil {
			return out, err
		}
		v.Order = orderAccounts(held[v.DeviceID], usage)
		if v.Order == nil {
			v.Order = []int64{}
		}
		if v.PendingSwitch == 0 {
			v.Suggestion = suggest(v.ActiveID, v.Order, usage, emails, now)
		}
		out.Devices = append(out.Devices, v)
	}
	return out, rows.Err()
}
