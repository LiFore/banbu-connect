package dist

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---- 管理服务：遥测查询（BXC-F0-028、BXC-F0-051、BXC-F0-052、BXC-F0-053）----
//
// 「客户端」维度在查询时算出来：
//   - 桌面客户端自己统计的流量（没有来源 IP）→ 这台客户端，键 d<id>；
//   - 路由盒统计的流量，来源 MAC 是某台桌面客户端上报的局域网地址 → 那台客户端（闸门直连时经路由盒代理，ADR-033）；
//   - 其余路由盒流量 → 路由盒下的设备，键 d<路由盒 id>:<MAC，没有时为 IP>。
// 流量只含经代理组的连接（DIRECT 不统计）；「未归属」单列。

// TelemetryRange 是查询的时间范围与趋势图的粒度。
type TelemetryRange struct {
	Name   string        `json:"name"`
	From   time.Time     `json:"from"`
	To     time.Time     `json:"to"`
	Bucket time.Duration `json:"-"`
	Step   string        `json:"step"` // minute / hour / day
}

// ParseRange 解析 1h / 24h / 7d / 30d，默认 24h。
func ParseRange(name string, now time.Time) TelemetryRange {
	r := TelemetryRange{Name: name, To: now.UTC()}
	switch name {
	case "1h":
		r.From, r.Bucket, r.Step = r.To.Add(-time.Hour), time.Minute, "minute"
	case "7d":
		r.From, r.Bucket, r.Step = r.To.AddDate(0, 0, -7), time.Hour, "hour"
	case "30d":
		r.From, r.Bucket, r.Step = r.To.AddDate(0, 0, -30), 24*time.Hour, "day"
	default:
		r.Name, r.From, r.Bucket, r.Step = "24h", r.To.Add(-24*time.Hour), time.Hour, "hour"
	}
	return r
}

// TelemetryFilter 是细粒度查看的筛选条件，空表示不限。
type TelemetryFilter struct {
	Client string `json:"client"`
	Host   string `json:"host"`
	Rule   string `json:"rule"` // 规则类型,规则内容
	Node   string `json:"node"` // 实际出站节点
	Group  string `json:"group"`
	Hop    string `json:"hop"` // 链路中任意一跳
}

// clientIndex 是客户端名称和桌面客户端的局域网 MAC。
type clientIndex struct {
	names  map[int64]string
	macOf  map[string]int64 // 局域网 MAC → 桌面客户端
	macsOf map[int64][]string
}

func (s *Server) clientIndex(ctx context.Context) (clientIndex, error) {
	ix := clientIndex{names: map[int64]string{}, macOf: map[string]int64{}, macsOf: map[int64][]string{}}
	rows, err := s.DB.QueryContext(ctx, `SELECT id, name FROM devices`)
	if err != nil {
		return ix, err
	}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return ix, err
		}
		ix.names[id] = name
	}
	rows.Close()
	rows, err = s.DB.QueryContext(ctx, `SELECT device_id, mac FROM device_lan`)
	if err != nil {
		return ix, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var mac string
		if err := rows.Scan(&id, &mac); err != nil {
			return ix, err
		}
		ix.macOf[mac] = id
		ix.macsOf[id] = append(ix.macsOf[id], mac)
	}
	return ix, rows.Err()
}

func (ix clientIndex) name(id int64) string {
	if n := ix.names[id]; n != "" {
		return n
	}
	return "#" + strconv.FormatInt(id, 10)
}

// client 算出一行数据属于哪个客户端（键和显示名）。
func (ix clientIndex) client(device int64, ip, mac, srcName string) (string, string) {
	if ip == "" && mac == "" {
		return "d" + strconv.FormatInt(device, 10), ix.name(device)
	}
	if id, ok := ix.macOf[mac]; ok && mac != "" {
		return "d" + strconv.FormatInt(id, 10), ix.name(id)
	}
	key := mac
	if key == "" {
		key = ip
	}
	label := srcName
	if label == "" {
		label = ip
	}
	return "d" + strconv.FormatInt(device, 10) + ":" + key, ix.name(device) + " · " + label
}

// filterSQL 把筛选条件转成 WHERE 子句（表需有 device_id、source_ip、source_mac、host、rule、payload、exit_node、grp、path 列）。
func (ix clientIndex) filterSQL(f TelemetryFilter) (string, []any) {
	var conds []string
	var args []any
	if f.Client != "" {
		dev, src, _ := strings.Cut(strings.TrimPrefix(f.Client, "d"), ":")
		id, _ := strconv.ParseInt(dev, 10, 64)
		if src == "" {
			c := "((device_id = ? AND source_ip = '' AND source_mac = '')"
			args = append(args, id)
			if macs := ix.macsOf[id]; len(macs) > 0 {
				c += " OR source_mac IN (" + strings.TrimSuffix(strings.Repeat("?,", len(macs)), ",") + ")"
				for _, m := range macs {
					args = append(args, m)
				}
			}
			conds = append(conds, c+")")
		} else {
			conds = append(conds, "(device_id = ? AND (source_mac = ? OR (source_mac = '' AND source_ip = ?)))")
			args = append(args, id, src, src)
		}
	}
	if f.Host != "" {
		conds, args = append(conds, "host = ?"), append(args, f.Host)
	}
	if f.Rule != "" {
		rule, payload, _ := strings.Cut(f.Rule, ",")
		conds, args = append(conds, "rule = ? AND payload = ?"), append(args, rule, payload)
	}
	if f.Node != "" {
		conds, args = append(conds, "exit_node = ?"), append(args, f.Node)
	}
	if f.Group != "" {
		conds, args = append(conds, "grp = ?"), append(args, f.Group)
	}
	if f.Hop != "" {
		b, _ := json.Marshal(f.Hop)
		conds, args = append(conds, "instr(path, ?) > 0"), append(args, string(b))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " AND " + strings.Join(conds, " AND "), args
}

// Traffic 是一段时间的代理流量。
type Traffic struct {
	Upload   int64 `json:"upload"`
	Download int64 `json:"download"`
	Conns    int64 `json:"conns"`
}

func (t *Traffic) add(o Traffic) {
	t.Upload += o.Upload
	t.Download += o.Download
	t.Conns += o.Conns
}

func (t Traffic) bytes() int64 { return t.Upload + t.Download }

// TrendPoint 是趋势图的一个点。
type TrendPoint struct {
	Time time.Time `json:"time"`
	Traffic
	Unattributed int64 `json:"unattributed"`
}

// RankItem 是排行的一项。Key 用于筛选（客户端键、主机、规则「类型,内容」、节点名）。
type RankItem struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Traffic
}

func bucketOf(t time.Time, r TelemetryRange) time.Time {
	if r.Step == "day" {
		l := t.Local()
		return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.Local).UTC()
	}
	return t.UTC().Truncate(r.Bucket)
}

// trend 按粒度汇总代理流量和未归属流量。
func (s *Server) trend(ctx context.Context, ix clientIndex, r TelemetryRange, f TelemetryFilter) ([]TrendPoint, error) {
	cond, args := ix.filterSQL(f)
	// 粒度是分钟时按分钟取，其余按小时取，再在本地时区里合并成天。
	unit := "minute"
	if r.Step != "minute" {
		unit = "substr(minute, 1, 13) || ':00:00Z'"
	}
	q := `SELECT ` + unit + `, SUM(upload), SUM(download), SUM(conns) FROM conn_minutes WHERE minute >= ? AND minute < ?` + cond + ` GROUP BY 1`
	rows, err := s.DB.QueryContext(ctx, q, append([]any{r.From.Format(time.RFC3339), r.To.Format(time.RFC3339)}, args...)...)
	if err != nil {
		return nil, err
	}
	points := map[time.Time]*TrendPoint{}
	get := func(t time.Time) *TrendPoint {
		b := bucketOf(t, r)
		p := points[b]
		if p == nil {
			p = &TrendPoint{Time: b}
			points[b] = p
		}
		return p
	}
	for rows.Next() {
		var at string
		var tr Traffic
		if err := rows.Scan(&at, &tr.Upload, &tr.Download, &tr.Conns); err != nil {
			rows.Close()
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339, at)
		get(t).add(tr)
	}
	rows.Close()
	// 未归属流量只按客户端筛选时有意义（筛域名、规则、节点时不显示）。
	if f.Host == "" && f.Rule == "" && f.Node == "" && f.Group == "" && f.Hop == "" {
		uq := `SELECT ` + unit + `, SUM(upload + download) FROM conn_unattributed WHERE minute >= ? AND minute < ?`
		uargs := []any{r.From.Format(time.RFC3339), r.To.Format(time.RFC3339)}
		if f.Client != "" {
			dev, src, _ := strings.Cut(strings.TrimPrefix(f.Client, "d"), ":")
			if src != "" {
				uq += " AND 0" // 路由盒下单个设备的未归属流量无法区分
			} else {
				uq += " AND device_id = ?"
				uargs = append(uargs, dev)
			}
		}
		rows, err = s.DB.QueryContext(ctx, uq+" GROUP BY 1", uargs...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var at string
			var n int64
			if err := rows.Scan(&at, &n); err != nil {
				rows.Close()
				return nil, err
			}
			t, _ := time.Parse(time.RFC3339, at)
			get(t).Unattributed += n
		}
		rows.Close()
	}
	// 补齐没有数据的点，折线不断开。
	out := []TrendPoint{}
	for t := bucketOf(r.From, r); !t.After(r.To); {
		if p := points[t]; p != nil {
			out = append(out, *p)
		} else {
			out = append(out, TrendPoint{Time: t})
		}
		if r.Step == "day" {
			t = bucketOf(t.Local().AddDate(0, 0, 1), r)
		} else {
			t = t.Add(r.Bucket)
		}
	}
	return out, nil
}

// rank 按一个维度汇总排行。dim 是 client / host / rule / node / group / process。
func (s *Server) rank(ctx context.Context, ix clientIndex, r TelemetryRange, f TelemetryFilter, dim string, limit int) ([]RankItem, error) {
	cond, args := ix.filterSQL(f)
	cols := map[string]string{
		"client":  "device_id, source_ip, source_mac, MAX(source_name)",
		"host":    "host",
		"rule":    "rule, payload",
		"node":    "exit_node",
		"group":   "grp",
		"process": "process",
	}[dim]
	groupBy := cols
	if dim == "client" {
		groupBy = "device_id, source_ip, source_mac"
	}
	q := `SELECT ` + cols + `, SUM(upload), SUM(download), SUM(conns) FROM conn_minutes WHERE minute >= ? AND minute < ?` + cond +
		` GROUP BY ` + groupBy
	rows, err := s.DB.QueryContext(ctx, q, append([]any{r.From.Format(time.RFC3339), r.To.Format(time.RFC3339)}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	merged := map[string]*RankItem{}
	for rows.Next() {
		var it RankItem
		var tr Traffic
		switch dim {
		case "client":
			var dev int64
			var ip, mac, name string
			if err := rows.Scan(&dev, &ip, &mac, &name, &tr.Upload, &tr.Download, &tr.Conns); err != nil {
				return nil, err
			}
			it.Key, it.Label = ix.client(dev, ip, mac, name)
		case "rule":
			var rule, payload string
			if err := rows.Scan(&rule, &payload, &tr.Upload, &tr.Download, &tr.Conns); err != nil {
				return nil, err
			}
			it.Key, it.Label = rule+","+payload, rule
			if payload != "" {
				it.Label += "," + payload
			}
		default:
			var v string
			if err := rows.Scan(&v, &tr.Upload, &tr.Download, &tr.Conns); err != nil {
				return nil, err
			}
			it.Key, it.Label = v, v
		}
		if m := merged[it.Key]; m != nil {
			m.add(tr)
		} else {
			it.Traffic = tr
			merged[it.Key] = &it
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]RankItem, 0, len(merged))
	for _, it := range merged {
		out = append(out, *it)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].bytes() != out[j].bytes() {
			return out[i].bytes() > out[j].bytes()
		}
		return out[i].Key < out[j].Key
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ---- 拓扑图（BXC-F0-052）----

// TopologyNode 是桑基图的一个节点。Kind 是 client / hop / host；Filter 是点击后带去细粒度查看的筛选。
type TopologyNode struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Depth  int             `json:"depth"`
	Filter TelemetryFilter `json:"filter"`
	Traffic
}

// TopologyLink 是一条连线。
type TopologyLink struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Traffic
}

// Topology 是拓扑图的数据。
type Topology struct {
	Nodes []TopologyNode `json:"nodes"`
	Links []TopologyLink `json:"links"`
}

// Topology 按 客户端 → 链路各跳节点 → 目标地址 汇总；每层只留前 limit 个，其余合并为「(其他)」。
// 链路的第一个是规则选中的代理组，链路长于 1 时不画它（只画节点）。
func (s *Server) Topology(ctx context.Context, r TelemetryRange, f TelemetryFilter, limit int) (Topology, error) {
	ix, err := s.clientIndex(ctx)
	if err != nil {
		return Topology{}, err
	}
	cond, args := ix.filterSQL(f)
	rows, err := s.DB.QueryContext(ctx, `SELECT device_id, source_ip, source_mac, MAX(source_name), path, host,
		SUM(upload), SUM(download), SUM(conns) FROM conn_minutes WHERE minute >= ? AND minute < ?`+cond+`
		GROUP BY device_id, source_ip, source_mac, path, host`,
		append([]any{r.From.Format(time.RFC3339), r.To.Format(time.RFC3339)}, args...)...)
	if err != nil {
		return Topology{}, err
	}
	type flow struct {
		client, label, host string
		hops                []string
		tr                  Traffic
	}
	var flows []flow
	clientTotal, hostTotal := map[string]int64{}, map[string]int64{}
	hopTotal := map[string]int64{}
	for rows.Next() {
		var fl flow
		var dev int64
		var ip, mac, name, path string
		if err := rows.Scan(&dev, &ip, &mac, &name, &path, &fl.host, &fl.tr.Upload, &fl.tr.Download, &fl.tr.Conns); err != nil {
			rows.Close()
			return Topology{}, err
		}
		fl.client, fl.label = ix.client(dev, ip, mac, name)
		_ = json.Unmarshal([]byte(path), &fl.hops)
		if len(fl.hops) > 1 {
			fl.hops = fl.hops[1:]
		}
		flows = append(flows, fl)
		clientTotal[fl.client] += fl.tr.bytes()
		hostTotal[fl.host] += fl.tr.bytes()
		for _, h := range fl.hops {
			hopTotal[h] += fl.tr.bytes()
		}
	}
	rows.Close()
	keepClient, keepHost, keepHop := topKeys(clientTotal, limit), topKeys(hostTotal, limit), topKeys(hopTotal, limit)

	nodes := map[string]*TopologyNode{}
	links := map[[2]string]*TopologyLink{}
	node := func(id, name, kind string, depth int, filter TelemetryFilter, tr Traffic) string {
		n := nodes[id]
		if n == nil {
			n = &TopologyNode{ID: id, Name: name, Kind: kind, Depth: depth, Filter: filter}
			nodes[id] = n
		}
		n.add(tr)
		return id
	}
	link := func(a, b string, tr Traffic) {
		l := links[[2]string{a, b}]
		if l == nil {
			l = &TopologyLink{Source: a, Target: b}
			links[[2]string{a, b}] = l
		}
		l.add(tr)
	}
	for _, fl := range flows {
		cid, cname, cf := "c:"+fl.client, fl.label, TelemetryFilter{Client: fl.client}
		if !keepClient[fl.client] {
			cid, cname, cf = "c:"+connStatsOtherLabel, connStatsOtherLabel, TelemetryFilter{}
		}
		prev := node(cid, cname, "client", 0, cf, fl.tr)
		for i, h := range fl.hops {
			id, name, hf := fmt.Sprintf("n%d:%s", i+1, h), h, TelemetryFilter{Hop: h}
			if !keepHop[h] {
				id, name, hf = fmt.Sprintf("n%d:%s", i+1, connStatsOtherLabel), connStatsOtherLabel, TelemetryFilter{}
			}
			cur := node(id, name, "hop", i+1, hf, fl.tr)
			link(prev, cur, fl.tr)
			prev = cur
		}
		hid, hname, ff := "h:"+fl.host, fl.host, TelemetryFilter{Host: fl.host}
		if !keepHost[fl.host] {
			hid, hname, ff = "h:"+connStatsOtherLabel, connStatsOtherLabel, TelemetryFilter{}
		}
		link(prev, node(hid, hname, "host", -1, ff, fl.tr), fl.tr)
	}
	out := Topology{Nodes: []TopologyNode{}, Links: []TopologyLink{}}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, *n)
	}
	for _, l := range links {
		out.Links = append(out.Links, *l)
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].ID < out.Nodes[j].ID })
	sort.Slice(out.Links, func(i, j int) bool {
		return out.Links[i].Source+"\x00"+out.Links[i].Target < out.Links[j].Source+"\x00"+out.Links[j].Target
	})
	return out, nil
}

const connStatsOtherLabel = "(其他)"

func topKeys(total map[string]int64, limit int) map[string]bool {
	keys := make([]string, 0, len(total))
	for k := range total {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if total[keys[i]] != total[keys[j]] {
			return total[keys[i]] > total[keys[j]]
		}
		return keys[i] < keys[j]
	})
	out := map[string]bool{}
	for i, k := range keys {
		if limit <= 0 || i < limit {
			out[k] = true
		}
	}
	return out
}

// ---- 总览（BXC-F0-051）与细粒度查看（BXC-F0-028）----

// TelemetryOverview 是遥测页的 Dashboard 数据（代理流量部分；Claude Code 用量见 otel 的接口）。
type TelemetryOverview struct {
	Range        TelemetryRange `json:"range"`
	Online       int            `json:"online"`       // 最近 5 分钟联系过管理服务的客户端
	Clients      int            `json:"clients"`      // 未吊销的客户端
	Active       int            `json:"active"`       // 这段时间有代理流量的客户端（路由盒下的设备分开算）
	Total        Traffic        `json:"total"`        // 代理流量
	Unattributed int64          `json:"unattributed"` // 未归属流量
	Trend        []TrendPoint   `json:"trend"`
	TopClients   []RankItem     `json:"top_clients"`
	TopHosts     []RankItem     `json:"top_hosts"`
	TopNodes     []RankItem     `json:"top_nodes"`
	TopRules     []RankItem     `json:"top_rules"`
}

func (s *Server) TelemetryOverview(ctx context.Context, r TelemetryRange) (TelemetryOverview, error) {
	o := TelemetryOverview{Range: r}
	ix, err := s.clientIndex(ctx)
	if err != nil {
		return o, err
	}
	since := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(last_seen >= ?), 0) FROM devices WHERE revoked = 0`, since).
		Scan(&o.Clients, &o.Online); err != nil {
		return o, err
	}
	if o.Trend, err = s.trend(ctx, ix, r, TelemetryFilter{}); err != nil {
		return o, err
	}
	for _, p := range o.Trend {
		o.Total.add(p.Traffic)
		o.Unattributed += p.Unattributed
	}
	all, err := s.rank(ctx, ix, r, TelemetryFilter{}, "client", 0)
	if err != nil {
		return o, err
	}
	o.Active, o.TopClients = len(all), all[:min(len(all), 10)]
	if o.TopHosts, err = s.rank(ctx, ix, r, TelemetryFilter{}, "host", 10); err != nil {
		return o, err
	}
	if o.TopNodes, err = s.rank(ctx, ix, r, TelemetryFilter{}, "node", 10); err != nil {
		return o, err
	}
	o.TopRules, err = s.rank(ctx, ix, r, TelemetryFilter{}, "rule", 10)
	return o, err
}

// TelemetryRecord 是细粒度查看里的一条连接记录。
type TelemetryRecord struct {
	ID          int64     `json:"id"`
	Client      string    `json:"client"`
	ClientLabel string    `json:"client_label"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Network     string    `json:"network"`
	Host        string    `json:"host"`
	Destination string    `json:"destination"`
	Rule        string    `json:"rule"`
	Payload     string    `json:"payload"`
	Path        []string  `json:"path"`
	Process     string    `json:"process,omitempty"`
	Upload      int64     `json:"upload"`
	Download    int64     `json:"download"`
}

// TelemetryDetail 是细粒度查看的数据。
type TelemetryDetail struct {
	Range     TelemetryRange    `json:"range"`
	Filter    TelemetryFilter   `json:"filter"`
	Total     Traffic           `json:"total"`
	Trend     []TrendPoint      `json:"trend"`
	Clients   []RankItem        `json:"clients"`
	Hosts     []RankItem        `json:"hosts"`
	Rules     []RankItem        `json:"rules"`
	Groups    []RankItem        `json:"groups"`
	Nodes     []RankItem        `json:"nodes"`
	Processes []RankItem        `json:"processes"`
	Records   []TelemetryRecord `json:"records"`
	// DeviceID 是筛选的客户端是一台客户端本身（不是路由盒下的设备）时它的 id，页面据此显示 Claude Code 会话。
	DeviceID int64 `json:"device_id,omitempty"`
}

const maxTelemetryRecords = 200

func (s *Server) TelemetryDetail(ctx context.Context, r TelemetryRange, f TelemetryFilter, before int64) (TelemetryDetail, error) {
	d := TelemetryDetail{Range: r, Filter: f, Records: []TelemetryRecord{}}
	ix, err := s.clientIndex(ctx)
	if err != nil {
		return d, err
	}
	if dev, src, _ := strings.Cut(strings.TrimPrefix(f.Client, "d"), ":"); f.Client != "" && src == "" {
		d.DeviceID, _ = strconv.ParseInt(dev, 10, 64)
	}
	if d.Trend, err = s.trend(ctx, ix, r, f); err != nil {
		return d, err
	}
	for _, p := range d.Trend {
		d.Total.add(p.Traffic)
	}
	for _, x := range []struct {
		dim string
		out *[]RankItem
	}{{"client", &d.Clients}, {"host", &d.Hosts}, {"rule", &d.Rules}, {"group", &d.Groups}, {"node", &d.Nodes}, {"process", &d.Processes}} {
		if *x.out, err = s.rank(ctx, ix, r, f, x.dim, 50); err != nil {
			return d, err
		}
	}
	cond, args := ix.filterSQL(f)
	q := `SELECT id, device_id, source_ip, source_mac, source_name, start_at, end_at, network, host, destination, rule, payload, path,
		process, upload, download FROM conn_records WHERE end_at >= ? AND end_at < ?` + cond
	qargs := append([]any{r.From.Format(time.RFC3339), r.To.Format(time.RFC3339)}, args...)
	if before > 0 {
		q += " AND id < ?"
		qargs = append(qargs, before)
	}
	rows, err := s.DB.QueryContext(ctx, q+" ORDER BY id DESC LIMIT ?", append(qargs, maxTelemetryRecords)...)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var rec TelemetryRecord
		var dev int64
		var ip, mac, name, start, end, path string
		if err := rows.Scan(&rec.ID, &dev, &ip, &mac, &name, &start, &end, &rec.Network, &rec.Host, &rec.Destination, &rec.Rule,
			&rec.Payload, &path, &rec.Process, &rec.Upload, &rec.Download); err != nil {
			return d, err
		}
		rec.Client, rec.ClientLabel = ix.client(dev, ip, mac, name)
		rec.Start, _ = time.Parse(time.RFC3339, start)
		rec.End, _ = time.Parse(time.RFC3339, end)
		_ = json.Unmarshal([]byte(path), &rec.Path)
		d.Records = append(d.Records, rec)
	}
	return d, rows.Err()
}

// ---- 流量用量（BXC-F0-053）----

// UsageSeries 是一个客户端每小时 / 每天的代理流量。
type UsageSeries struct {
	Key    string       `json:"key"`
	Label  string       `json:"label"`
	Total  Traffic      `json:"total"`
	Points []TrendPoint `json:"points"`
}

// Usage 从长期保留的小时用量里汇总每个客户端（路由盒细到设备）每小时或每天的代理流量。step 为 hour 或 day。
func (s *Server) Usage(ctx context.Context, from, to time.Time, step string) ([]UsageSeries, error) {
	ix, err := s.clientIndex(ctx)
	if err != nil {
		return nil, err
	}
	r := TelemetryRange{From: from.UTC(), To: to.UTC(), Bucket: time.Hour, Step: "hour"}
	if step == "day" {
		r.Bucket, r.Step = 24*time.Hour, "day"
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT device_id, source_key, source_name, hour, upload, download, conns FROM conn_hours
		WHERE hour >= ? AND hour < ? ORDER BY hour`, r.From.Truncate(time.Hour).Format(time.RFC3339), r.To.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	series := map[string]*UsageSeries{}
	points := map[string]map[time.Time]*TrendPoint{}
	for rows.Next() {
		var dev int64
		var key, name, hour string
		var tr Traffic
		if err := rows.Scan(&dev, &key, &name, &hour, &tr.Upload, &tr.Download, &tr.Conns); err != nil {
			return nil, err
		}
		ip, mac := key, ""
		if strings.Contains(key, ":") && !strings.Contains(key, ".") && len(key) == 17 {
			ip, mac = "", key
		}
		ck, label := ix.client(dev, ip, mac, name)
		u := series[ck]
		if u == nil {
			u = &UsageSeries{Key: ck, Label: label}
			series[ck], points[ck] = u, map[time.Time]*TrendPoint{}
		}
		u.Total.add(tr)
		t, _ := time.Parse(time.RFC3339, hour)
		b := bucketOf(t, r)
		p := points[ck][b]
		if p == nil {
			p = &TrendPoint{Time: b}
			points[ck][b] = p
		}
		p.add(tr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]UsageSeries, 0, len(series))
	for k, u := range series {
		for _, p := range points[k] {
			u.Points = append(u.Points, *p)
		}
		sort.Slice(u.Points, func(i, j int) bool { return u.Points[i].Time.Before(u.Points[j].Time) })
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Total.bytes() > out[j].Total.bytes() })
	return out, nil
}
