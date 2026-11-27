package otel

import (
	"context"
	"database/sql"
	"sort"
	"time"
)

// 统计口径（与 open-agentcode 相同）：成本和 tokens 取 api_request 事件（每次请求一条，带 session.id）；
// 代码行、提交、活跃时长取 delta 指标（Claude Code 默认按 delta 导出，累计值不参与求和）。

// Tokens 是各类 token 数。
type Tokens struct {
	Input         float64 `json:"input"`
	Output        float64 `json:"output"`
	CacheRead     float64 `json:"cache_read"`
	CacheCreation float64 `json:"cache_creation"`
}

// Totals 是一段时间的汇总。
type Totals struct {
	CostUSD      float64 `json:"cost_usd"`
	Requests     int64   `json:"requests"`
	Tokens       Tokens  `json:"tokens"`
	Sessions     int64   `json:"sessions"`
	Prompts      int64   `json:"prompts"`
	ToolCalls    int64   `json:"tool_calls"`
	ToolFailures int64   `json:"tool_failures"`
	APIErrors    int64   `json:"api_errors"`
	LinesAdded   float64 `json:"lines_added"`
	LinesRemoved float64 `json:"lines_removed"`
	Commits      float64 `json:"commits"`
	ActiveSecs   float64 `json:"active_seconds"`
}

// ModelUsage 是按模型的汇总。
type ModelUsage struct {
	Model    string  `json:"model"`
	Requests int64   `json:"requests"`
	CostUSD  float64 `json:"cost_usd"`
	Tokens   float64 `json:"tokens"`
	P50Ms    float64 `json:"p50_ms"`
	P95Ms    float64 `json:"p95_ms"`
}

// AccountUsage 是按 Claude 账号（事件里的 user.email）的汇总：多开时区分各工作区的用量（BXC-F0-049）。
type AccountUsage struct {
	Email    string  `json:"email"`
	Requests int64   `json:"requests"`
	CostUSD  float64 `json:"cost_usd"`
	Tokens   float64 `json:"tokens"`
	Sessions int64   `json:"sessions"`
}

// DayUsage 是按天（本地时区）的汇总。
type DayUsage struct {
	Day      string  `json:"day"`
	CostUSD  float64 `json:"cost_usd"`
	Tokens   float64 `json:"tokens"`
	Requests int64   `json:"requests"`
}

// SessionUsage 是一个会话的汇总。
type SessionUsage struct {
	SessionID string  `json:"session_id"`
	Cwd       string  `json:"cwd"`
	FirstMs   int64   `json:"first_ms"`
	LastMs    int64   `json:"last_ms"`
	CostUSD   float64 `json:"cost_usd"`
	Prompts   int64   `json:"prompts"`
	ToolCalls int64   `json:"tool_calls"`
}

// Summary 是统计页的全部数据。
type Summary struct {
	SinceMs  int64          `json:"since_ms"`
	UntilMs  int64          `json:"until_ms"`
	Totals   Totals         `json:"totals"`
	Models   []ModelUsage   `json:"models"`
	Accounts []AccountUsage `json:"accounts"`
	Days     []DayUsage     `json:"days"`
	Sessions []SessionUsage `json:"sessions"`
}

// AllDevices 传给 Summarize 表示汇总所有客户端（管理服务遥测页的总览）。
const AllDevices int64 = -1

// devCond 是按设备筛选的条件，参数要传两次设备：AllDevices 时匹配所有客户端（device_id > 0）。
const devCond = "(device_id = ? or (? = -1 and device_id > 0))"

func j(key string) string { return "cast(json_extract(attrs, '$.\"" + key + "\"') as real)" }

const tokensExpr = "coalesce(cast(json_extract(attrs, '$.input_tokens') as real), 0) + coalesce(cast(json_extract(attrs, '$.output_tokens') as real), 0)" +
	" + coalesce(cast(json_extract(attrs, '$.cache_read_tokens') as real), 0) + coalesce(cast(json_extract(attrs, '$.cache_creation_tokens') as real), 0)"

// Summarize 汇总一台设备最近 days 天（含今天）的数据；客户端自己的数据 device 为 0。
func (s *Store) Summarize(ctx context.Context, device int64, days int) (Summary, error) {
	if days <= 0 {
		days = 7
	}
	now := s.now()
	y, m, d := now.Date()
	since := time.Date(y, m, d, 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(days - 1))
	out := Summary{SinceMs: since.UnixMilli(), UntilMs: now.UnixMilli(), Models: []ModelUsage{}, Accounts: []AccountUsage{}, Days: []DayUsage{},
		Sessions: []SessionUsage{}}
	from, to := out.SinceMs, out.UntilMs+1

	t := &out.Totals
	var cost, in, outTok, cr, cc sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, `select count(*), sum(`+j("cost_usd")+`), sum(`+j("input_tokens")+`), sum(`+j("output_tokens")+`),
		sum(`+j("cache_read_tokens")+`), sum(`+j("cache_creation_tokens")+`)
		from otel_logs where `+devCond+` and name = 'api_request' and ts_ms >= ? and ts_ms < ?`, device, device, from, to).
		Scan(&t.Requests, &cost, &in, &outTok, &cr, &cc); err != nil {
		return out, err
	}
	t.CostUSD, t.Tokens = cost.Float64, Tokens{in.Float64, outTok.Float64, cr.Float64, cc.Float64}

	var prompts, calls, fails, errs sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `select count(distinct session_id), sum(name = 'user_prompt'), sum(name = 'tool_result'),
		sum(name = 'tool_result' and json_extract(attrs, '$.success') in ('false', 0)), sum(name = 'api_error')
		from otel_logs where `+devCond+` and ts_ms >= ? and ts_ms < ?`, device, device, from, to).Scan(&t.Sessions, &prompts, &calls, &fails, &errs); err != nil {
		return out, err
	}
	t.Prompts, t.ToolCalls, t.ToolFailures, t.APIErrors = prompts.Int64, calls.Int64, fails.Int64, errs.Int64

	metric := func(name, typ string) (float64, error) {
		q := `select sum(value) from otel_metrics where ` + devCond + ` and name = ? and cumulative = 0 and end_ms >= ? and end_ms < ?`
		args := []any{device, device, name, from, to}
		if typ != "" {
			q += ` and json_extract(attrs, '$.type') = ?`
			args = append(args, typ)
		}
		var v sql.NullFloat64
		err := s.db.QueryRowContext(ctx, q, args...).Scan(&v)
		return v.Float64, err
	}
	var err error
	if t.LinesAdded, err = metric("claude_code.lines_of_code.count", "added"); err != nil {
		return out, err
	}
	if t.LinesRemoved, err = metric("claude_code.lines_of_code.count", "removed"); err != nil {
		return out, err
	}
	if t.Commits, err = metric("claude_code.commit.count", ""); err != nil {
		return out, err
	}
	if t.ActiveSecs, err = metric("claude_code.active_time.total", ""); err != nil {
		return out, err
	}

	if out.Models, err = s.byModel(ctx, device, from, to); err != nil {
		return out, err
	}
	if out.Accounts, err = s.byAccount(ctx, device, from, to); err != nil {
		return out, err
	}
	if out.Days, err = s.byDay(ctx, device, since, days, from, to); err != nil {
		return out, err
	}
	out.Sessions, err = s.recentSessions(ctx, device, from, to, 20)
	return out, err
}

// byAccount 按事件里的 user.email 汇总；没有邮箱的（API key 登录等）记为空。
func (s *Store) byAccount(ctx context.Context, device, from, to int64) ([]AccountUsage, error) {
	rows, err := s.db.QueryContext(ctx, `select coalesce(json_extract(attrs, '$."user.email"'), ''), sum(name = 'api_request'),
		coalesce(sum(case when name = 'api_request' then `+j("cost_usd")+` end), 0),
		coalesce(sum(case when name = 'api_request' then `+tokensExpr+` end), 0), count(distinct session_id)
		from otel_logs where `+devCond+` and ts_ms >= ? and ts_ms < ? group by 1 order by 3 desc`, device, device, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccountUsage{}
	for rows.Next() {
		var a AccountUsage
		if err := rows.Scan(&a.Email, &a.Requests, &a.CostUSD, &a.Tokens, &a.Sessions); err != nil {
			return nil, err
		}
		if a.Requests > 0 || a.Sessions > 0 {
			out = append(out, a)
		}
	}
	return out, rows.Err()
}

func (s *Store) byModel(ctx context.Context, device, from, to int64) ([]ModelUsage, error) {
	rows, err := s.db.QueryContext(ctx, `select coalesce(json_extract(attrs, '$.model'), '未知'), coalesce(`+j("duration_ms")+`, 0),
		coalesce(`+j("cost_usd")+`, 0), `+tokensExpr+` from otel_logs where `+devCond+` and name = 'api_request' and ts_ms >= ? and ts_ms < ?`, device, device, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type acc struct {
		ModelUsage
		durations []float64
	}
	byName := map[string]*acc{}
	for rows.Next() {
		var model string
		var dur, cost, tokens float64
		if err := rows.Scan(&model, &dur, &cost, &tokens); err != nil {
			return nil, err
		}
		a := byName[model]
		if a == nil {
			a = &acc{ModelUsage: ModelUsage{Model: model}}
			byName[model] = a
		}
		a.Requests++
		a.CostUSD += cost
		a.Tokens += tokens
		a.durations = append(a.durations, dur)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []ModelUsage{}
	for _, a := range byName {
		sort.Float64s(a.durations)
		a.P50Ms, a.P95Ms = percentile(a.durations, 50), percentile(a.durations, 95)
		out = append(out, a.ModelUsage)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].CostUSD > out[k].CostUSD })
	return out, nil
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p / 100)
	return sorted[i]
}

func (s *Store) byDay(ctx context.Context, device int64, since time.Time, days int, from, to int64) ([]DayUsage, error) {
	rows, err := s.db.QueryContext(ctx, `select ts_ms, coalesce(`+j("cost_usd")+`, 0), `+tokensExpr+`
		from otel_logs where `+devCond+` and name = 'api_request' and ts_ms >= ? and ts_ms < ?`, device, device, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// 按 Go 的本地时区分天：SQLite 的 localtime 在服务（SYSTEM/root）里不一定是用户的时区。
	out := make([]DayUsage, days)
	index := map[string]int{}
	for i := range out {
		day := since.AddDate(0, 0, i).Format("2006-01-02")
		out[i].Day = day
		index[day] = i
	}
	for rows.Next() {
		var ts int64
		var cost, tokens float64
		if err := rows.Scan(&ts, &cost, &tokens); err != nil {
			return nil, err
		}
		if i, ok := index[time.UnixMilli(ts).In(since.Location()).Format("2006-01-02")]; ok {
			out[i].CostUSD += cost
			out[i].Tokens += tokens
			out[i].Requests++
		}
	}
	return out, rows.Err()
}

func (s *Store) recentSessions(ctx context.Context, device, from, to int64, limit int) ([]SessionUsage, error) {
	rows, err := s.db.QueryContext(ctx, `select s.session_id, coalesce(s.cwd, ''), s.first_ms, s.last_ms,
		coalesce((select sum(`+j("cost_usd")+`) from otel_logs l where l.device_id = s.device_id and l.session_id = s.session_id and l.name = 'api_request'), 0),
		(select count(*) from otel_logs l where l.device_id = s.device_id and l.session_id = s.session_id and l.name = 'user_prompt'),
		(select count(*) from otel_logs l where l.device_id = s.device_id and l.session_id = s.session_id and l.name = 'tool_result')
		from otel_sessions s where (s.device_id = ? or (? = -1 and s.device_id > 0)) and s.last_ms >= ? and s.last_ms < ?
		order by s.last_ms desc limit ?`, device, device, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionUsage{}
	for rows.Next() {
		var u SessionUsage
		if err := rows.Scan(&u.SessionID, &u.Cwd, &u.FirstMs, &u.LastMs, &u.CostUSD, &u.Prompts, &u.ToolCalls); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeviceUsage 是一台客户端在一段时间里的汇总（管理服务的总览用）。
type DeviceUsage struct {
	DeviceID int64   `json:"device_id"`
	CostUSD  float64 `json:"cost_usd"`
	Requests int64   `json:"requests"`
	Tokens   float64 `json:"tokens"`
	Sessions int64   `json:"sessions"`
	LastMs   int64   `json:"last_ms"`
}

// Devices 汇总各客户端最近 days 天的用量（不含 device 0，即管理服务自己）。
func (s *Store) Devices(ctx context.Context, days int) ([]DeviceUsage, error) {
	if days <= 0 {
		days = 7
	}
	now := s.now()
	y, m, d := now.Date()
	from := time.Date(y, m, d, 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(days - 1)).UnixMilli()
	rows, err := s.db.QueryContext(ctx, `select device_id,
		coalesce(sum(case when name = 'api_request' then `+j("cost_usd")+` end), 0),
		sum(name = 'api_request'),
		coalesce(sum(case when name = 'api_request' then `+tokensExpr+` end), 0),
		count(distinct session_id), max(ts_ms)
		from otel_logs where device_id > 0 and ts_ms >= ? group by device_id order by 2 desc`, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeviceUsage{}
	for rows.Next() {
		var u DeviceUsage
		if err := rows.Scan(&u.DeviceID, &u.CostUSD, &u.Requests, &u.Tokens, &u.Sessions, &u.LastMs); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
