package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"time"

	"boxctl/internal/dist"
)

// 一键刷新规则（概览页）：绑定了管理服务时立即拉一次下发的配置（规则、规则集、线路都在里面），
// 版本变了就重新生成内核配置；然后让内核重新下载所有远程规则集，比较前后的规则条数。
// 结果告诉页面有没有更新，页面据此明显提示。

// RuleSetRefresh 是一个远程规则集的刷新结果。
type RuleSetRefresh struct {
	Name   string `json:"name"`
	Before int    `json:"before"`
	After  int    `json:"after"`
	Error  string `json:"error,omitempty"`
}

// RulesRefresh 是一次刷新的结果。
type RulesRefresh struct {
	Bound         bool             `json:"bound"`
	BeforeVersion int64            `json:"before_version"`
	AfterVersion  int64            `json:"after_version"`
	ProfileError  string           `json:"profile_error,omitempty"`
	CoreRunning   bool             `json:"core_running"`
	RuleSets      []RuleSetRefresh `json:"rule_sets"`
	Updated       bool             `json:"updated"` // 下发的配置有新版本，或有规则集的条数变了
	CheckedAt     time.Time        `json:"checked_at"`
}

type ruleProviders struct {
	Providers map[string]struct {
		Name      string `json:"name"`
		RuleCount int    `json:"ruleCount"`
		Vehicle   string `json:"vehicleType"`
	} `json:"providers"`
}

// ruleProviderCounts 返回内核里各远程规则集的规则条数；内核没在运行时 ok 为 false。
func (s *Supervisor) ruleProviderCounts(ctx context.Context) (map[string]int, bool) {
	code, body, err := s.CallCore(ctx, http.MethodGet, "/providers/rules", nil)
	var rp ruleProviders
	if err != nil || code != http.StatusOK || json.Unmarshal(body, &rp) != nil {
		return nil, false
	}
	counts := map[string]int{}
	for name, p := range rp.Providers {
		if p.Vehicle == "Inline" {
			continue // 内联规则集没有地址可下载
		}
		counts[name] = p.RuleCount
	}
	return counts, true
}

// RefreshRules 执行一次刷新。c 为 nil 或没有绑定时只刷新规则集。
func RefreshRules(ctx context.Context, c *dist.Client, s *Supervisor) RulesRefresh {
	res := RulesRefresh{CheckedAt: time.Now().UTC()}
	if c != nil {
		st := c.Status()
		res.Bound, res.BeforeVersion, res.AfterVersion = st.Bound, st.Version, st.Version
		if st.Bound {
			changed, err := c.Sync(ctx)
			if err != nil {
				res.ProfileError = err.Error()
			}
			res.AfterVersion = c.Status().Version
			if changed {
				res.Updated = true
				s.Trigger()
			}
		}
	}

	before, ok := s.ruleProviderCounts(ctx)
	res.CoreRunning = ok
	res.RuleSets = []RuleSetRefresh{}
	if !ok {
		return res
	}
	names := make([]string, 0, len(before))
	for n := range before {
		names = append(names, n)
	}
	sort.Strings(names)
	errs := map[string]string{}
	for _, n := range names {
		code, body, err := s.CallCore(ctx, http.MethodPut, "/providers/rules/"+url.PathEscape(n), nil)
		switch {
		case err != nil:
			errs[n] = err.Error()
		case code != http.StatusNoContent && code != http.StatusOK:
			var e struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(body, &e)
			if e.Message == "" {
				e.Message = http.StatusText(code)
			}
			errs[n] = e.Message
		}
	}
	after, _ := s.ruleProviderCounts(ctx)
	for _, n := range names {
		r := RuleSetRefresh{Name: n, Before: before[n], After: after[n], Error: errs[n]}
		if r.Error == "" && r.After != r.Before {
			res.Updated = true
		}
		res.RuleSets = append(res.RuleSets, r)
	}
	return res
}

// RulesRefreshRoutes 挂 POST /api/v1/<prefix>/rules/refresh。
func RulesRefreshRoutes(mux *http.ServeMux, authed Authed, prefix string, c *dist.Client, s *Supervisor) {
	mux.Handle("POST /api/v1/"+prefix+"/rules/refresh", authed(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		WriteJSON(w, http.StatusOK, RefreshRules(ctx, c, s))
	}))
}
