package panel

import (
	"html/template"
	"net/http"

	"boxctl/internal/buildinfo"
)

// 管理服务的落地页：根路径只显示版本信息，管理后台在 /admin。
// 不经前端：没构建前端或前端出错时也能确认服务在跑、是哪个版本。

// AdminBase 是管理服务后台页面的路径前缀，与 web/src/main.tsx 的 basename 一致。
const AdminBase = "/admin"

var landingTmpl = template.Must(template.New("landing").Parse(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>Banbu Connect</title>
<style>
:root { color-scheme: light dark; --fg: #1f2329; --muted: #646a73; --bg: #f5f6f7; --card: #fff; --line: #e4e6eb; }
@media (prefers-color-scheme: dark) { :root { --fg: #e8eaed; --muted: #9aa0a6; --bg: #17181a; --card: #222326; --line: #34363a; } }
body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: var(--bg); color: var(--fg);
  font: 14px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif; }
main { background: var(--card); border: 1px solid var(--line); border-radius: 12px; padding: 28px 32px; margin: 16px; min-width: 260px; }
h1 { font-size: 18px; margin: 0 0 16px; }
dl { display: grid; grid-template-columns: auto 1fr; gap: 6px 16px; margin: 0; }
dt { color: var(--muted); }
dd { margin: 0; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; word-break: break-all; }
</style>
</head>
<body>
<main>
<h1>Banbu Connect 管理服务</h1>
<dl>
<dt>版本</dt><dd>{{.Version}}</dd>
<dt>提交</dt><dd>{{.Commit}}</dd>
<dt>构建时间</dt><dd>{{.Date}}</dd>
</dl>
</main>
</body>
</html>
`))

func landing(w http.ResponseWriter, r *http.Request) {
	info := struct{ Version, Commit, Date string }{buildinfo.Version, buildinfo.Commit, buildinfo.Date}
	// 要 JSON 的（监控、脚本）直接给 JSON。
	if r.URL.Query().Get("format") == "json" {
		writeJSON(w, http.StatusOK, map[string]string{"version": info.Version, "commit": info.Commit, "date": info.Date})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_ = landingTmpl.Execute(w, info)
}
