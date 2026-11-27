package dist

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"boxctl/internal/gen"
	"boxctl/internal/killswitch"
)

// 规则预览（M5-06）：按草稿模拟客户端的规则顺序——下发规则、受保护服务（客户端默认）、远程规则集、兜底，
// 离线判断一个域名或 IP 命中哪条规则、走哪个出口组。规则集内容由管理服务下载，和页面查看共用缓存。
// 指定方案时按那个方案（筛选后的代理组、规则、规则集、线路选择）模拟。

// SimulateRequest 是模拟请求。
type SimulateRequest struct {
	Query   string `json:"query"`
	Port    int    `json:"port"`
	Network string `json:"network"`
	// ProfileID 是要模拟的方案；0 表示「默认」。
	ProfileID int64 `json:"profile_id"`
}

// SimulateRules 按当前草稿模拟。
func (s *Server) SimulateRules(ctx context.Context, in SimulateRequest) (gen.SimResult, error) {
	q, err := gen.ParseQuery(in.Query, in.Port, in.Network)
	if err != nil {
		return gen.SimResult{}, &InputError{err.Error()}
	}
	var p RuleProfile
	if in.ProfileID != 0 {
		p, err = s.ruleProfile(ctx, in.ProfileID)
	} else {
		p, err = s.defaultProfile(ctx)
	}
	if err != nil {
		return gen.SimResult{}, err
	}
	ns, err := s.Nodes.Export(ctx)
	if err != nil {
		return gen.SimResult{}, err
	}
	gs, err := s.Groups.Export(ctx)
	if err != nil {
		return gen.SimResult{}, err
	}
	sc := scope(ns, gs, p.ProfileContent)
	rules, sets := scopeRules(p.Rules, p.RuleSets, sc.Excluded)
	if sets == nil {
		sets = gen.DefaultRuleSets()
	}
	groupNames := map[string]bool{}
	for _, g := range sc.Groups {
		groupNames[g.Name] = true
	}

	var list []gen.SimRule
	fallback := ""
	for _, r := range rules {
		if strings.HasPrefix(strings.ToUpper(r), "MATCH,") {
			fallback = strings.TrimSpace(r[len("MATCH,"):])
			continue
		}
		list = append(list, gen.SimRule{Rule: r, Source: "下发规则"})
	}
	for _, svc := range killswitch.DefaultServices() {
		for _, suf := range svc.Suffixes {
			list = append(list,
				gen.SimRule{Rule: "AND,((DOMAIN-SUFFIX," + suf + "),(NETWORK,udp)),REJECT", Source: "受保护服务「" + svc.Name + "」（客户端默认）"},
				gen.SimRule{Rule: "DOMAIN-SUFFIX," + suf + "," + svc.Group, Source: "受保护服务「" + svc.Name + "」（客户端默认）"})
		}
	}
	contents := map[string]gen.RuleSetContent{}
	for _, rs := range sets {
		if rs.Disabled || !(groupNames[rs.Target] || rs.Target == "DIRECT" || rs.Target == "REJECT") {
			continue // 目标不存在的规则集客户端会跳过
		}
		rule := "RULE-SET," + rs.Name + "," + rs.Target
		if rs.Type == gen.TypeGeoIP {
			rule = "GEOIP," + rs.Name + "," + rs.Target
		} else {
			contents[rs.Name] = s.cachedRuleSet(ctx, rs)
		}
		if rs.NoResolve {
			rule += ",no-resolve"
		}
		list = append(list, gen.SimRule{Rule: rule, Source: "远程规则集"})
	}
	switch {
	case fallback != "":
		list = append(list, gen.SimRule{Rule: "MATCH," + fallback, Source: "兜底（下发）"})
	case groupNames["漏网之鱼"]:
		list = append(list, gen.SimRule{Rule: "MATCH,漏网之鱼", Source: "兜底（默认）"})
	default:
		list = append(list, gen.SimRule{Rule: "MATCH,DIRECT", Source: "兜底（默认）"})
	}
	res := gen.Simulate(q, list, contents)
	if res.Match != nil {
		res.Match.Final = sc.Selections[res.Match.Target]
	}
	return res, nil
}

// cachedRuleSet 取规则集内容（和页面查看共用缓存，ADR-064）。
func (s *Server) cachedRuleSet(ctx context.Context, rs gen.RuleSet) gen.RuleSetContent {
	c := gen.RuleSetContent{Behavior: rs.Behavior}
	if rs.Format == "mrs" {
		c.Err = "mrs 是二进制格式"
	} else if got, err := s.loadRuleSet(ctx, rs.URL, rs.Format, false); err != nil {
		c.Err = err.Error()
	} else {
		c.Lines = got.Rules
	}
	return c
}

func (s *Server) simulateRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, log *slog.Logger) {
	mux.Handle("POST /api/v1/dist/rules/simulate", authed(func(w http.ResponseWriter, r *http.Request) {
		var in SimulateRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			writeErr(w, &InputError{"请求内容无效"}, log)
			return
		}
		res, err := s.SimulateRules(r.Context(), in)
		if err != nil {
			writeErr(w, err, log)
			return
		}
		writeJSON(w, http.StatusOK, res)
	}))
}
