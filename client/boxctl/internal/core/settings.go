package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"boxctl/internal/groups"
)

// Settings 是内核运行设置，存在 settings 表里。桌面客户端和路由盒各用一个键。
type Settings struct {
	Mode        string   `json:"mode"` // rule / global / direct
	TUN         bool     `json:"tun"`
	Running     bool     `json:"running"` // 期望状态：服务重启后是否自动启动内核
	MixedPort   int      `json:"mixed_port"`
	CustomRules []string `json:"custom_rules"`
	// DNSGuard 在 TUN 接管时拦截物理网卡上的 DNS（53、853），防止系统和应用绕过 TUN 查询。只用于桌面。
	DNSGuard bool `json:"dns_guard"`
	// AppliedProfile 是已经应用过线路选择的下发版本，由程序维护。
	AppliedProfile int64 `json:"applied_profile"`
	// Onboarded 表示已经走完首次启动的引导屏。
	Onboarded bool `json:"onboarded"`
	// Diagnostics 表示绑定管理服务后上报诊断数据，默认打开。只用于桌面。
	Diagnostics bool `json:"diagnostics"`
	// ConnStats 表示绑定管理服务后统计并上报经代理组的连接（S1.8，ADR-048），默认打开，在「服务端」页可关闭。
	ConnStats bool `json:"conn_stats"`
	// Mirror 是规则集、geo 数据的下载镜像（与 jsDelivr 路径兼容），空表示不用（M7-04）。
	Mirror string `json:"mirror"`
	// AllowLAN 让局域网设备手动填路由盒的代理端口（mixed-port）上网，默认关闭。只用于路由盒。
	AllowLAN bool `json:"allow_lan"`
}

// DefaultCustomRules 是桌面和路由盒共用的默认自定义规则。
func DefaultCustomRules() []string {
	return []string{
		"DST-PORT,19302,REJECT",
		"DOMAIN-KEYWORD,thinkphp1,REJECT",
		"DOMAIN-KEYWORD,adobe,REJECT",
		"AND,((DOMAIN-KEYWORD,anthropic),(NETWORK,udp)),REJECT",
		"AND,((DOMAIN-KEYWORD,claude),(NETWORK,udp)),REJECT",
		"DOMAIN-SUFFIX,ping0.cc,Claude",
		"DOMAIN-KEYWORD,anthropic,Claude",
		"DOMAIN-KEYWORD,claude,Claude",
		"DOMAIN,servd-anthropic-website.b-cdn.net,Claude",
	}
}

// Store 读写一份 Settings。
type Store struct {
	DB       *sql.DB
	Key      string
	Defaults func() Settings
}

func (s Store) Load(ctx context.Context) (Settings, error) {
	var raw string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, s.Key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return s.Defaults(), nil
	}
	if err != nil {
		return Settings{}, err
	}
	st := s.Defaults()
	// 有了这个字段之前就保存过设置的，是已经在用的安装，不再弹引导屏。
	st.Onboarded = true
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return Settings{}, err
	}
	return st, nil
}

func (s Store) Save(ctx context.Context, st Settings) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now')`, s.Key, string(b))
	return err
}

const seededKey = "desktop_seeded"

// SeedGroups 第一次运行时写入默认代理组，之后用户可以在界面上随意修改。
// 结构来自原来的规则脚本；成员用动态筛选，订阅更新后自动跟上。
// 桌面和路由盒共用同一个数据库键：一份数据库只写一次。
func SeedGroups(ctx context.Context, db *sql.DB, svc *groups.Service) error {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key = ?`, seededKey).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	all := groups.Filter{Enabled: true, ExcludeTags: []string{"isp"}}
	create := func(g groups.Group) (string, error) {
		v, err := svc.Create(ctx, g)
		if err != nil {
			return "", fmt.Errorf("创建默认代理组「%s」：%w", g.Name, err)
		}
		return v.ID, nil
	}
	autoOpts := groups.Options{Tolerance: 50, Hidden: true}

	manual, err := create(groups.Group{Name: "手动选择", Type: groups.TypeSelect, Members: groups.Members{Filter: all}})
	if err != nil {
		return err
	}
	allAuto, err := create(groups.Group{Name: "ALL - 自动选择", Type: groups.TypeURLTest, Members: groups.Members{Filter: all}, Options: autoOpts})
	if err != nil {
		return err
	}
	auto := []string{allAuto}
	regional := []string{}
	for _, region := range []string{"香港", "台湾", "新加坡", "日本", "美国"} {
		f := groups.Filter{Enabled: true, Regions: []string{region}, ExcludeTags: []string{"isp"}}
		id, err := create(groups.Group{Name: region + " - 自动选择", Type: groups.TypeURLTest, Members: groups.Members{Filter: f}, Options: autoOpts})
		if err != nil {
			return err
		}
		auto = append(auto, id)
		regional = append(regional, id)
	}
	autoGroup, err := create(groups.Group{Name: "自动选择", Type: groups.TypeSelect, Members: groups.Members{Explicit: auto}})
	if err != nil {
		return err
	}
	hash, err := create(groups.Group{Name: "负载均衡", Type: groups.TypeLoadBalance, Members: groups.Members{Filter: all},
		Options: groups.Options{Strategy: "consistent-hashing", MaxFailedTimes: 3, Lazy: true}})
	if err != nil {
		return err
	}
	mode, err := create(groups.Group{Name: "代理模式", Type: groups.TypeSelect,
		Members: groups.Members{Explicit: []string{autoGroup, manual, hash, groups.Direct}}})
	if err != nil {
		return err
	}
	// 严格组不引用含 DIRECT 的「代理模式」：任何路径都不能走到直连。
	for _, name := range []string{"Claude", "ChatGPT"} {
		if _, err := create(groups.Group{Name: name, Type: groups.TypeSelect, Strict: true,
			Members: groups.Members{Explicit: []string{autoGroup, manual}}}); err != nil {
			return err
		}
	}
	for _, name := range []string{"电报消息", "Spotify"} {
		if _, err := create(groups.Group{Name: name, Type: groups.TypeSelect,
			Members: groups.Members{Explicit: append([]string{mode}, regional...)}}); err != nil {
			return err
		}
	}
	if _, err := create(groups.Group{Name: "漏网之鱼", Type: groups.TypeSelect,
		Members: groups.Members{Explicit: []string{groups.Direct, mode}}}); err != nil {
		return err
	}
	if _, err := create(groups.Group{Name: "广告拦截", Type: groups.TypeSelect,
		Members: groups.Members{Explicit: []string{groups.Reject, groups.Direct, mode}}}); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, '1')`, seededKey)
	return err
}
