package desktop

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"boxctl/internal/dist"
)

// ---- AI 账号在线时长上报 ----
//
// 每 10 秒看一次本机此刻在用的账号，变化时立即上报，没变化时每分钟上报一次（管理服务据此判断客户端还在线）：
//   - Gemini：本机当前账号（手动切换、额度将尽自动轮换、管理页面下发切换都会改它）；
//   - Claude / Codex：有进程在运行的工作区（设为默认的工作区按默认目录的进程算）；拿不到进程数时（Windows）
//     按「设为默认」的账号算；默认目录里登录的是托管账号但没交给工作区的，也按默认目录的进程算。
// 外壳超过 2 分钟没有报来扫描结果（没在运行）时，不知道 Claude / Codex 的进程，只报 Gemini。
// 管理服务按上报维护时段，切走的在上报时结束，客户端离线的在最后一次上报时结束（dist/ai_online.go）。

const (
	aiOnlineTick      = 10 * time.Second
	aiOnlineHeartbeat = time.Minute
	aiOnlineShellGone = 2 * time.Minute
)

// onlineAccounts 算出此刻在用的账号。
func onlineAccounts(sync dist.AISync, synced bool, gemini int64, r *WorkspaceReport, now time.Time) []dist.AIOnlineAccount {
	var out []dist.AIOnlineAccount
	if gemini != 0 {
		out = append(out, dist.AIOnlineAccount{Kind: dist.KindGemini, AccountID: gemini})
	}
	if !synced || r == nil || now.Sub(r.At) > aiOnlineShellGone {
		return out
	}
	ids := map[string]int64{}
	for _, a := range sync.Accounts {
		ids[tokenKey(a.Kind, strings.ToLower(a.Email))] = a.ID
	}
	seen := map[int64]bool{}
	add := func(kind, email string) {
		if id, ok := ids[tokenKey(kind, strings.ToLower(email))]; ok && !seen[id] {
			seen[id] = true
			out = append(out, dist.AIOnlineAccount{Kind: kind, AccountID: id})
		}
	}
	for _, ws := range r.Workspaces {
		if ws.Running > 0 || (ws.Running < 0 && ws.Default) {
			add(ws.Kind, ws.Email)
		}
	}
	for kind, d := range r.Defaults {
		if d.Owner == "" && d.LoggedIn && d.Email != "" && (d.Running > 0 || d.Running < 0) {
			add(kind, d.Email)
		}
	}
	slices.SortFunc(out, func(a, b dist.AIOnlineAccount) int {
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return int(a.AccountID - b.AccountID)
	})
	return out
}

// runAIOnline 持续上报在用的账号，直到 ctx 结束。
func runAIOnline(ctx context.Context, c *dist.Client, ws *workspaces, log *slog.Logger) {
	var last []dist.AIOnlineAccount
	var lastAt time.Time
	reported := false
	backoff := time.Time{} // 管理服务是旧版本（没有接口）时，过一阵再试
	t := time.NewTicker(aiOnlineTick)
	defer t.Stop()
	for {
		now := time.Now()
		if !c.Status().Bound {
			reported = false
		} else if now.After(backoff) {
			sync, synced := c.AISyncState()
			cur := onlineAccounts(sync, synced, c.GeminiActive(), ws.Report(), now)
			if !reported || !slices.Equal(cur, last) || now.Sub(lastAt) >= aiOnlineHeartbeat {
				err := c.ReportAIOnline(ctx, cur)
				var he *dist.HTTPError
				switch {
				case err == nil:
					last, lastAt, reported = cur, now, true
				case errors.As(err, &he) && he.Status == http.StatusNotFound:
					backoff = now.Add(30 * time.Minute)
				case ctx.Err() == nil:
					log.Debug("上报 AI 账号在线状态失败", "err", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
