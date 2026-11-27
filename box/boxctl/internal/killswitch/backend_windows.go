package killswitch

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf16"
)

// Windows：NRPT 负责 DNS 层，系统防火墙负责 IP 层。都是持久配置，不依赖本进程。
//
// TODO(verify:V4) Apply、Remove 还没有在真实机器上执行过；见 client/README.md。
// TODO(verify:V5) 防火墙关闭的配置文件上 IP 层不生效，Inspect 会给出警告。

const psPrelude = `$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
$Tag = 'boxguard'
`

const psInterfaces = `ConvertTo-Json -Compress @(Get-NetAdapter -Physical | ForEach-Object { $_.Name })`

const psInspect = `
$nrpt = @(Get-DnsClientNrptRule | Where-Object { $_.Comment -eq $Tag })
$fw = @(Get-NetFirewallRule -Group $Tag -ErrorAction SilentlyContinue)
ConvertTo-Json -Compress @{
  namespaces = @($nrpt | ForEach-Object { $_.Namespace })
  servers = @($nrpt | ForEach-Object { $_.NameServers } | ForEach-Object { "$_" })
  firewall = @($fw | ForEach-Object { @{ description = $_.Description; enabled = "$($_.Enabled)"; action = "$($_.Action)" } })
  profiles_off = @(Get-NetFirewallProfile | Where-Object { "$($_.Enabled)" -eq 'False' } | ForEach-Object { $_.Name })
}
`

// 先加新规则再删旧规则：更新过程中不留没有保护的窗口。新规则加失败时旧规则保持不动。
const psApply = `
$d = $env:KS_DESIRED | ConvertFrom-Json
$oldNrpt = @(Get-DnsClientNrptRule | Where-Object { $_.Comment -eq $Tag })
$oldFw = @(Get-NetFirewallRule -Group $Tag -ErrorAction SilentlyContinue)
if (@($d.namespaces).Count -gt 0) {
  Add-DnsClientNrptRule -Namespace @($d.namespaces) -NameServers $d.server -Comment $Tag | Out-Null
}
if (@($d.cidrs).Count -gt 0) {
  New-NetFirewallRule -Name ("$Tag-" + [guid]::NewGuid()) -DisplayName "$Tag protected CIDRs" -Group $Tag -Description $d.hash -Direction Outbound -Action Block -Protocol Any -Profile Any -RemoteAddress @($d.cidrs) -InterfaceAlias @($d.interfaces) | Out-Null
}
$oldNrpt | ForEach-Object { Remove-DnsClientNrptRule -Name $_.Name -Force }
$oldFw | Remove-NetFirewallRule
Clear-DnsClientCache
`

const psRemove = `
Get-DnsClientNrptRule | Where-Object { $_.Comment -eq $Tag } | ForEach-Object { Remove-DnsClientNrptRule -Name $_.Name -Force }
Get-NetFirewallRule -Group $Tag -ErrorAction SilentlyContinue | Remove-NetFirewallRule
Clear-DnsClientCache
`

type windowsBackend struct{}

func NewSystemBackend() Backend { return windowsBackend{} }

func powershellPath() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
}

func runPS(ctx context.Context, script string, env ...string) (string, error) {
	u := utf16.Encode([]rune(psPrelude + script))
	raw := make([]byte, 2*len(u))
	for i, c := range u {
		raw[2*i], raw[2*i+1] = byte(c), byte(c>>8)
	}
	cmd := exec.CommandContext(ctx, powershellPath(), "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", base64.StdEncoding.EncodeToString(raw))
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, firstLine(stderr.String()))
	}
	return stdout.String(), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func (windowsBackend) Interfaces(ctx context.Context) ([]string, error) {
	out, err := runPS(ctx, psInterfaces)
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		return nil, fmt.Errorf("解析网卡列表失败：%w", err)
	}
	return names, nil
}

type psFirewallRule struct {
	Description string `json:"description"`
	Enabled     string `json:"enabled"`
	Action      string `json:"action"`
}

type psInspectResult struct {
	Namespaces  []string         `json:"namespaces"`
	Servers     []string         `json:"servers"`
	Firewall    []psFirewallRule `json:"firewall"`
	ProfilesOff []string         `json:"profiles_off"`
}

func (windowsBackend) Inspect(ctx context.Context, d Desired) (Actual, error) {
	out, err := runPS(ctx, psInspect)
	if err != nil {
		return Actual{}, err
	}
	var r psInspectResult
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return Actual{}, fmt.Errorf("解析系统规则失败：%w", err)
	}
	return compareWindows(r, d), nil
}

// compareWindows 比较系统规则和期望，平台无关逻辑单独拿出来便于测试。
func compareWindows(r psInspectResult, d Desired) Actual {
	a := Actual{Installed: len(r.Namespaces) > 0 || len(r.Firewall) > 0}
	want := nrptNamespaces(d.Suffixes)
	got := slices.Clone(r.Namespaces)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		a.Diffs = append(a.Diffs, "DNS 路由规则与配置不一致")
	}
	for _, s := range r.Servers {
		if s != d.ListenIP {
			a.Diffs = append(a.Diffs, "DNS 路由指向了 "+s)
			break
		}
	}
	switch {
	case len(d.CIDRs) == 0:
		if len(r.Firewall) > 0 {
			a.Diffs = append(a.Diffs, "存在多余的防火墙规则")
		}
	case len(r.Firewall) != 1:
		a.Diffs = append(a.Diffs, "防火墙规则缺失或重复")
	default:
		rule := r.Firewall[0]
		if rule.Description != firewallHash(d) {
			a.Diffs = append(a.Diffs, "防火墙规则的网段或网卡有变化")
		}
		if rule.Enabled != "True" || rule.Action != "Block" {
			a.Diffs = append(a.Diffs, "防火墙规则被禁用或改动")
		}
	}
	if len(d.CIDRs) > 0 && len(r.ProfilesOff) > 0 {
		a.Warnings = append(a.Warnings, "Windows 防火墙这些配置文件是关闭的，IP 层在这些网络上不生效："+strings.Join(r.ProfilesOff, "、"))
	}
	a.InSync = len(a.Diffs) == 0
	return a
}

func (windowsBackend) Apply(ctx context.Context, d Desired) error {
	payload, err := json.Marshal(map[string]any{
		"namespaces": nrptNamespaces(d.Suffixes),
		"server":     d.ListenIP,
		"cidrs":      d.CIDRs,
		"interfaces": d.Interfaces,
		"hash":       firewallHash(d),
	})
	if err != nil {
		return err
	}
	_, err = runPS(ctx, psApply, "KS_DESIRED="+string(payload))
	return err
}

func (windowsBackend) Remove(ctx context.Context) error {
	_, err := runPS(ctx, psRemove)
	return err
}

func (windowsBackend) FlushDNS(ctx context.Context) error {
	_, err := runPS(ctx, "Clear-DnsClientCache")
	return err
}

func (windowsBackend) ValidateListen(_ string, port int) error {
	if port != 53 {
		return &ValidationError{"Windows 的 NRPT 不能指定端口，DNS 入口必须用 53 端口，如 127.0.0.2:53"}
	}
	return nil
}
