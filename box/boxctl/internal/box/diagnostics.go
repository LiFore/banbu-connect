package box

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"boxctl/internal/buildinfo"
	"boxctl/internal/core"
	"boxctl/internal/guard"
	"boxctl/internal/ipc"
	"boxctl/internal/logring"
)

// 诊断包（M11-04）：一键导出 tar.gz，含状态、配置（凭据打码）、guard 规则、日志和系统信息。
// 每个文件写入前都经过脱敏（MaskSecrets + logring.Redact）：订阅地址的令牌、节点密码、控制接口密钥都不会出现。
// 订阅地址、管理服务令牌、设备密钥、数据库本身不进诊断包。

type diagFile struct {
	name string
	data []byte
}

func (b *Box) diagnostics(ctx context.Context) ([]byte, error) {
	var files []diagFile
	add := func(name string, data []byte) {
		files = append(files, diagFile{name, []byte(MaskSecrets(logring.Redact(string(data))))})
	}
	addJSON := func(name string, v any) {
		j, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			j = []byte(err.Error())
		}
		add(name, j)
	}

	sum := b.summary(ctx)
	if sum.Server != nil {
		s := *sum.Server
		s.URL = redactHost(s.URL)
		sum.Server = &s
	}
	addJSON("status.json", sum)
	if y := b.sup.AppliedConfig(); len(y) > 0 {
		add("config.yaml", y)
	}
	if p, err := b.Policy(ctx); err == nil {
		addJSON("guard-policy.json", p)
	}
	var ruleset string
	rctx, cancel := context.WithTimeout(ctx, ipcTimeout)
	if err := ipc.Call(rctx, b.opts.GuardSocket, guard.MethodRuleset, nil, &ruleset); err != nil {
		ruleset = "读取失败：" + err.Error()
	}
	cancel()
	add("guard-ruleset.txt", []byte(ruleset))
	if v, err := b.listVersions(ctx); err == nil {
		addJSON("versions.json", v)
	}
	if d, err := b.listDevices(ctx); err == nil {
		addJSON("devices.json", d)
	}
	addJSON("rule-hits.json", b.monitor.RuleHits())
	addJSON("strict-expansion.json", b.expand.Result())

	logText := func(list []logring.Entry) []byte {
		var s strings.Builder
		for _, e := range list {
			fmt.Fprintf(&s, "%s %-5s %s\n", e.Time.Format(time.RFC3339), e.Level, e.Message)
		}
		return []byte(s.String())
	}
	add("logs/panel.log", logText(b.panelLogs("")))
	if gl, err := b.guardLogs(ctx, ""); err == nil {
		add("logs/guard.log", logText(gl))
	}
	add("logs/core.log", logText(b.coreLogs("")))
	add("system.txt", b.systemInfo(ctx))

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	now := time.Now()
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: "boxctl-diag/" + f.name, Mode: 0o600, Size: int64(len(f.data)), ModTime: now}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// redactHost 只保留管理服务地址的主机名，去掉路径和参数。
func redactHost(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			rest = rest[:j]
		}
		return u[:i+3] + rest
	}
	return u
}

// systemInfo 收集系统信息；命令不存在（开发机）时跳过。
func (b *Box) systemInfo(ctx context.Context) []byte {
	var s strings.Builder
	fmt.Fprintf(&s, "boxctl %s\n%s/%s\n\n", buildinfo.String(), runtime.GOOS, runtime.GOARCH)
	if rel, err := os.ReadFile("/etc/openwrt_release"); err == nil {
		s.Write(rel)
		s.WriteString("\n")
	}
	cmds := [][]string{
		{"uname", "-a"},
		{"uptime"},
		{"free"},
		{"uci", "-q", "show", "firewall.@defaults[0]"},
		{"uci", "-q", "show", "dhcp.lan"},
		{"ip", "-4", "addr"},
		{"ip", "-4", "route", "show", "table", "all"},
		{"ip", "rule"},
		{"nft", "list", "tables"},
	}
	for _, c := range cmds {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		out, err := exec.CommandContext(cctx, c[0], c[1:]...).CombinedOutput()
		cancel()
		if err != nil && len(out) == 0 {
			continue
		}
		fmt.Fprintf(&s, "$ %s\n%s\n", strings.Join(c, " "), out)
	}
	return []byte(s.String())
}

func (b *Box) diagnosticsRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /api/v1/box/diagnostics", authed(func(w http.ResponseWriter, r *http.Request) {
		data, err := b.diagnostics(r.Context())
		if err != nil {
			core.Fail(w, err)
			return
		}
		name := "boxctl-diag-" + time.Now().Format("20060102-150405") + ".tar.gz"
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		_, _ = w.Write(data)
	}))
}
