package box

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"boxctl/internal/core"
	"boxctl/internal/guard"
)

// 局域网设备（M-设备）：从 dnsmasq 租约和邻居表发现设备，按设备设置出口、断网、时段、绕过内核和自定义规则。
//   - 出口、断网、时段、自定义规则按 IP（ADR-034、ADR-036、ADR-041），IP 变了策略不跟随，界面提示绑定静态地址；
//   - 绕过内核按 MAC（ADR-040），手机私有 MAC 轮换会失效；
//   - 「全部直连」「绕过内核」的设备访问受保护服务会被 guard 阻断（ADR-035）。

const (
	EgressDefault = "default"
	EgressDirect  = "direct"
	EgressProxy   = "proxy"
	EgressGroup   = "group"

	AccessAllow    = "allow"
	AccessBlock    = "block"
	AccessSchedule = "schedule"

	// proxyGroup 是「全部代理」用的代理组。
	proxyGroup = "代理模式"
	// maxDeviceRules 是一台设备的自定义规则条数上限。
	maxDeviceRules = 200
)

// Policy 是一个 IP 的设备策略。
type DevicePolicy struct {
	IP          string         `json:"ip"`
	Egress      string         `json:"egress"`
	EgressGroup string         `json:"egress_group,omitempty"`
	Access      string         `json:"access"`
	Schedule    []guard.Window `json:"schedule"`
	Rules       []string       `json:"rules"`
}

// Device 是设备列表的一行。
type Device struct {
	MAC        string        `json:"mac,omitempty"`
	IP         string        `json:"ip,omitempty"`
	Hostname   string        `json:"hostname,omitempty"`
	Name       string        `json:"name,omitempty"` // 备注名
	Online     bool          `json:"online"`
	State      string        `json:"state,omitempty"` // 邻居表状态
	LeaseUntil *time.Time    `json:"lease_until,omitempty"`
	PrivateMAC bool          `json:"private_mac"` // 本地管理地址：多为手机的私有地址，可能轮换
	Bypass     bool          `json:"bypass"`
	Policy     *DevicePolicy `json:"policy,omitempty"`
	Usage      *DeviceUsage  `json:"usage,omitempty"`
	Client     *BoxClient    `json:"client,omitempty"` // 这台设备上的 Banbu Connect 客户端（clients.go）
}

type lease struct {
	mac, ip, host string
	until         time.Time
}

type neighbor struct{ ip, mac, state string }

// discovery 读租约和邻居表；路径和命令可替换，便于测试。
type discovery struct {
	leasesPath string
	neighbors  func(ctx context.Context) []neighbor
}

func defaultDiscovery(lanDev string) discovery {
	return discovery{leasesPath: "/tmp/dhcp.leases", neighbors: func(ctx context.Context) []neighbor {
		if runtime.GOOS != "linux" {
			return nil
		}
		args := []string{"-4", "neigh", "show"}
		if lanDev != "" {
			args = append(args, "dev", lanDev)
		}
		out, err := exec.CommandContext(ctx, "ip", args...).Output()
		if err != nil {
			return nil
		}
		return parseNeighbors(string(out))
	}}
}

// parseLeases 解析 dnsmasq 租约文件：到期时间 MAC IP 主机名 客户端ID。
func parseLeases(data string) []lease {
	var out []lease
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 || strings.Contains(f[2], ":") { // 跳过 DHCPv6 租约
			continue
		}
		mac, err := net.ParseMAC(f[1])
		if err != nil {
			continue
		}
		l := lease{mac: mac.String(), ip: f[2]}
		if f[3] != "*" {
			l.host = f[3]
		}
		if sec, err := strconv.ParseInt(f[0], 10, 64); err == nil && sec > 0 {
			l.until = time.Unix(sec, 0).UTC()
		}
		out = append(out, l)
	}
	return out
}

// parseNeighbors 解析 `ip -4 neigh show`：IP [dev X] lladdr MAC 状态。
func parseNeighbors(data string) []neighbor {
	var out []neighbor
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		n := neighbor{ip: f[0], state: f[len(f)-1]}
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "lladdr" {
				if mac, err := net.ParseMAC(f[i+1]); err == nil {
					n.mac = mac.String()
				}
			}
		}
		if n.mac == "" {
			continue // FAILED / INCOMPLETE
		}
		out = append(out, n)
	}
	return out
}

func privateMAC(mac string) bool {
	m, err := net.ParseMAC(mac)
	return err == nil && len(m) > 0 && m[0]&0x02 != 0
}

// ---- 存储 ----

func (b *Box) devicePolicies(ctx context.Context) (map[string]DevicePolicy, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT ip, egress, egress_group, access, schedule, rules FROM lan_policies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]DevicePolicy{}
	for rows.Next() {
		var p DevicePolicy
		var sched, rules string
		if err := rows.Scan(&p.IP, &p.Egress, &p.EgressGroup, &p.Access, &sched, &rules); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(sched), &p.Schedule)
		_ = json.Unmarshal([]byte(rules), &p.Rules)
		if p.Schedule == nil {
			p.Schedule = []guard.Window{}
		}
		if p.Rules == nil {
			p.Rules = []string{}
		}
		out[p.IP] = p
	}
	return out, rows.Err()
}

type deviceNote struct {
	name   string
	bypass bool
}

func (b *Box) deviceNotes(ctx context.Context) (map[string]deviceNote, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT mac, name, bypass FROM lan_devices`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]deviceNote{}
	for rows.Next() {
		var mac string
		var n deviceNote
		if err := rows.Scan(&mac, &n.name, &n.bypass); err != nil {
			return nil, err
		}
		out[mac] = n
	}
	return out, rows.Err()
}

// normalizePolicy 校验设备策略。groups 是现有代理组名。
func normalizePolicy(p DevicePolicy, groups map[string]bool) (DevicePolicy, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(p.IP))
	if err != nil || !a.Is4() {
		return p, errors.New("设备地址须是 IPv4")
	}
	p.IP = a.String()
	switch p.Egress {
	case "", EgressDefault:
		p.Egress, p.EgressGroup = EgressDefault, ""
	case EgressDirect, EgressProxy:
		p.EgressGroup = ""
	case EgressGroup:
		if !groups[p.EgressGroup] {
			return p, fmt.Errorf("代理组「%s」不存在", p.EgressGroup)
		}
	default:
		return p, errors.New("出口须是 default、direct、proxy 或 group")
	}
	switch p.Access {
	case "", AccessAllow:
		p.Access, p.Schedule = AccessAllow, nil
	case AccessBlock:
		p.Schedule = nil
	case AccessSchedule:
		if len(p.Schedule) == 0 {
			return p, errors.New("按时段放行至少要有一个时段")
		}
	default:
		return p, errors.New("上网控制须是 allow、block 或 schedule")
	}
	// 时段格式由 guard 校验，规则一致。
	if _, err := (guard.Policy{Blocks: []guard.Block{{IP: p.IP, Windows: p.Schedule}}}).Normalize(); err != nil {
		return p, err
	}
	var rules []string
	for _, r := range p.Rules {
		r = strings.TrimSpace(r)
		if r == "" || strings.HasPrefix(r, "#") {
			continue
		}
		if !strings.Contains(r, ",") {
			return p, fmt.Errorf("规则格式不对：%s", r)
		}
		if strings.HasPrefix(strings.ToUpper(r), "MATCH,") || strings.HasPrefix(strings.ToUpper(r), "SUB-RULE,") {
			return p, fmt.Errorf("设备规则里不能用 MATCH、SUB-RULE：%s（没命中的会继续按全局规则走）", r)
		}
		rules = append(rules, r)
	}
	if len(rules) > maxDeviceRules {
		return p, fmt.Errorf("一台设备最多 %d 条规则", maxDeviceRules)
	}
	if rules == nil {
		rules = []string{}
	}
	if p.Schedule == nil {
		p.Schedule = []guard.Window{}
	}
	p.Rules = rules
	return p, nil
}

func isDefaultPolicy(p DevicePolicy) bool {
	return p.Egress == EgressDefault && p.Access == AccessAllow && len(p.Rules) == 0
}

// ---- 生成配置和 guard 策略用 ----

func subRuleName(ip string) string { return "dev-" + ip }

// deviceConfig 返回设备的出口规则、子规则和不经内核的 MAC（交给生成器）。
func (b *Box) deviceConfig(ctx context.Context) (rules []string, sub map[string][]string, excludeMAC []string) {
	policies, err := b.devicePolicies(ctx)
	if err != nil {
		b.log.Warn("读取设备策略失败", "err", err)
		return nil, nil, nil
	}
	ips := make([]string, 0, len(policies))
	for ip := range policies {
		ips = append(ips, ip)
	}
	sort.Slice(ips, func(i, j int) bool { return netip.MustParseAddr(ips[i]).Less(netip.MustParseAddr(ips[j])) })
	for _, ip := range ips {
		p := policies[ip]
		src := "SRC-IP-CIDR," + ip + "/32"
		// 自定义规则在出口之前：没命中的再按设备出口（或全局规则）走（ADR-041）。
		if len(p.Rules) > 0 {
			if sub == nil {
				sub = map[string][]string{}
			}
			sub[subRuleName(ip)] = p.Rules
			rules = append(rules, "SUB-RULE,("+src+"),"+subRuleName(ip))
		}
		switch p.Egress {
		case EgressDirect:
			rules = append(rules, src+",DIRECT")
		case EgressProxy:
			rules = append(rules, src+","+proxyGroup)
		case EgressGroup:
			rules = append(rules, src+","+p.EgressGroup)
		}
	}
	notes, err := b.deviceNotes(ctx)
	if err == nil {
		for mac, n := range notes {
			if n.bypass {
				excludeMAC = append(excludeMAC, mac)
			}
		}
		sort.Strings(excludeMAC)
	}
	return rules, sub, excludeMAC
}

// deviceBlocks 返回断网和时段管控（交给 guard）。
func (b *Box) deviceBlocks(ctx context.Context) []guard.Block {
	policies, err := b.devicePolicies(ctx)
	if err != nil {
		return nil
	}
	var out []guard.Block
	for _, p := range policies {
		switch p.Access {
		case AccessBlock:
			out = append(out, guard.Block{IP: p.IP})
		case AccessSchedule:
			out = append(out, guard.Block{IP: p.IP, Windows: p.Schedule})
		}
	}
	return out
}

// ---- 列表 ----

func (b *Box) listDevices(ctx context.Context) ([]Device, error) {
	notes, err := b.deviceNotes(ctx)
	if err != nil {
		return nil, err
	}
	policies, err := b.devicePolicies(ctx)
	if err != nil {
		return nil, err
	}
	byMAC := map[string]*Device{}
	var order []string
	get := func(mac string) *Device {
		d := byMAC[mac]
		if d == nil {
			d = &Device{MAC: mac, PrivateMAC: privateMAC(mac)}
			byMAC[mac] = d
			order = append(order, mac)
		}
		return d
	}
	if data, err := os.ReadFile(b.discovery.leasesPath); err == nil {
		for _, l := range parseLeases(string(data)) {
			d := get(l.mac)
			d.IP, d.Hostname = l.ip, l.host
			if !l.until.IsZero() {
				u := l.until
				d.LeaseUntil = &u
			}
		}
	}
	for _, n := range b.discovery.neighbors(ctx) {
		d := get(n.mac)
		if d.IP == "" {
			d.IP = n.ip
		}
		d.State = n.state
		d.Online = n.state == "REACHABLE" || n.state == "DELAY" || n.state == "PROBE" || n.state == "STALE" && d.IP == n.ip
	}
	// 只有备注、没在租约和邻居表里出现的设备也列出来（离线）。
	for mac := range notes {
		get(mac)
	}
	usage := map[string]DeviceUsage{}
	if b.monitor != nil {
		usage = b.monitor.Devices()
	}
	entry := map[string]int{}
	for ip, u := range usage {
		entry[ip] = u.Entry
	}
	clients := b.clients.snapshot(entry)
	out := []Device{}
	usedIP := map[string]bool{}
	for _, mac := range order {
		d := byMAC[mac]
		if n, ok := notes[mac]; ok {
			d.Name, d.Bypass = n.name, n.bypass
		}
		if p, ok := policies[d.IP]; ok && d.IP != "" {
			p := p
			d.Policy, usedIP[d.IP] = &p, true
		}
		if u, ok := usage[d.IP]; ok && d.IP != "" {
			u := u
			d.Usage = &u
		}
		out = append(out, *d)
	}
	// 设置了策略、但现在没有设备用这个 IP：单列出来，提示 IP 可能变了。
	for ip, p := range policies {
		if !usedIP[ip] {
			p := p
			out = append(out, Device{IP: ip, Policy: &p})
		}
	}
	// 接入的客户端按 IP 挂到对应的设备上；租约和邻居表里没有的（跨网段经代理入口接入、ADR-084）单列一行。
	for i := range out {
		d := &out[i]
		if c, ok := clients[d.IP]; ok && d.IP != "" {
			c := c
			d.Client = &c
			d.Online = d.Online || c.Online
			delete(clients, d.IP)
		}
	}
	for ip, c := range clients {
		c := c
		d := Device{IP: ip, Online: c.Online, Client: &c}
		if u, ok := usage[ip]; ok {
			u := u
			d.Usage = &u
		}
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		ai, aerr := netip.ParseAddr(out[i].IP)
		bi, berr := netip.ParseAddr(out[j].IP)
		if aerr != nil || berr != nil {
			return aerr == nil
		}
		return ai.Less(bi)
	})
	return out, nil
}

// ---- 接口 ----

func (b *Box) deviceRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
	changed := func() {
		b.sup.Trigger() // 出口、自定义规则、绕过内核 → 重新生成配置
		go b.push()     // 断网、时段 → guard
	}
	mux.Handle("GET /api/v1/box/devices", authed(func(w http.ResponseWriter, r *http.Request) {
		list, err := b.listDevices(r.Context())
		if err != nil {
			core.Fail(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, list)
	}))
	mux.Handle("PUT /api/v1/box/devices/{mac}", authed(func(w http.ResponseWriter, r *http.Request) {
		mac, err := net.ParseMAC(r.PathValue("mac"))
		if err != nil {
			core.WriteJSON(w, http.StatusBadRequest, core.APIErr{Code: "bad_request", Message: "MAC 地址无效"})
			return
		}
		var in struct {
			Name   string `json:"name"`
			Bypass bool   `json:"bypass"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			core.WriteJSON(w, http.StatusBadRequest, core.APIErr{Code: "bad_request", Message: "请求内容无效"})
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if len([]rune(in.Name)) > 64 {
			core.WriteJSON(w, http.StatusBadRequest, core.APIErr{Code: "invalid", Message: "备注名最长 64 字"})
			return
		}
		if in.Name == "" && !in.Bypass {
			_, err = b.db.ExecContext(r.Context(), `DELETE FROM lan_devices WHERE mac = ?`, mac.String())
		} else {
			_, err = b.db.ExecContext(r.Context(), `INSERT INTO lan_devices (mac, name, bypass) VALUES (?, ?, ?)
				ON CONFLICT(mac) DO UPDATE SET name = excluded.name, bypass = excluded.bypass,
				updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now')`, mac.String(), in.Name, in.Bypass)
		}
		if err != nil {
			core.Fail(w, err)
			return
		}
		changed()
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.Handle("PUT /api/v1/box/policies/{ip}", authed(func(w http.ResponseWriter, r *http.Request) {
		var in DevicePolicy
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			core.WriteJSON(w, http.StatusBadRequest, core.APIErr{Code: "bad_request", Message: "请求内容无效"})
			return
		}
		in.IP = r.PathValue("ip")
		gs, err := b.groupNames(r.Context())
		if err != nil {
			core.Fail(w, err)
			return
		}
		p, err := normalizePolicy(in, gs)
		if err != nil {
			core.WriteJSON(w, http.StatusBadRequest, core.APIErr{Code: "invalid", Message: err.Error()})
			return
		}
		if isDefaultPolicy(p) {
			_, err = b.db.ExecContext(r.Context(), `DELETE FROM lan_policies WHERE ip = ?`, p.IP)
		} else {
			if err := b.checkBlockLimit(r.Context(), p); err != nil {
				core.WriteJSON(w, http.StatusBadRequest, core.APIErr{Code: "invalid", Message: err.Error()})
				return
			}
			sched, _ := json.Marshal(p.Schedule)
			rules, _ := json.Marshal(p.Rules)
			_, err = b.db.ExecContext(r.Context(), `INSERT INTO lan_policies (ip, egress, egress_group, access, schedule, rules)
				VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(ip) DO UPDATE SET egress = excluded.egress, egress_group = excluded.egress_group,
				access = excluded.access, schedule = excluded.schedule, rules = excluded.rules,
				updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now')`, p.IP, p.Egress, p.EgressGroup, p.Access, string(sched), string(rules))
		}
		if err != nil {
			core.Fail(w, err)
			return
		}
		changed()
		core.WriteJSON(w, http.StatusOK, p)
	}))
	mux.Handle("DELETE /api/v1/box/policies/{ip}", authed(func(w http.ResponseWriter, r *http.Request) {
		if _, err := b.db.ExecContext(r.Context(), `DELETE FROM lan_policies WHERE ip = ?`, r.PathValue("ip")); err != nil {
			core.Fail(w, err)
			return
		}
		changed()
		w.WriteHeader(http.StatusNoContent)
	}))
}

func (b *Box) checkBlockLimit(ctx context.Context, p DevicePolicy) error {
	if p.Access == AccessAllow {
		return nil
	}
	var n int
	err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM lan_policies WHERE access != 'allow' AND ip != ?`, p.IP).Scan(&n)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if n >= guard.MaxBlocks {
		return fmt.Errorf("最多对 %d 台设备做上网控制", guard.MaxBlocks)
	}
	return nil
}

func (b *Box) groupNames(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	gs, err := b.groups.Export(ctx)
	if err != nil {
		return nil, err
	}
	for _, g := range gs {
		out[g.Name] = true
	}
	if b.dist != nil {
		if p, ok := b.dist.Profile(); ok {
			for _, g := range p.Groups {
				out[g.Name] = true
			}
		}
	}
	return out, nil
}
