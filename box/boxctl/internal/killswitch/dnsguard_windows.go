package killswitch

import (
	"context"
	"encoding/json"
	"strings"
)

// Windows：单独分组的出站防火墙规则，只作用于物理网卡，不影响回环上的 NRPT 目标。
// 防火墙规则默认持久，服务崩溃后由下次启动撤销（服务恢复动作几秒内拉起）。

const psGuardApply = `
$ifs = $env:KS_GUARD | ConvertFrom-Json
$old = @(Get-NetFirewallRule -Group 'boxguard-dns' -ErrorAction SilentlyContinue)
foreach ($p in 'UDP','TCP') {
  New-NetFirewallRule -Name ('boxguard-dns-' + [guid]::NewGuid()) -DisplayName "boxguard DNS leak $p" -Group 'boxguard-dns' -Direction Outbound -Action Block -Protocol $p -RemotePort 53,853 -Profile Any -InterfaceAlias @($ifs) | Out-Null
}
$old | Remove-NetFirewallRule
`

const psGuardRemove = `Get-NetFirewallRule -Group 'boxguard-dns' -ErrorAction SilentlyContinue | Remove-NetFirewallRule`

const psGuardActive = `@(Get-NetFirewallRule -Group 'boxguard-dns' -ErrorAction SilentlyContinue | Where-Object { "$($_.Enabled)" -eq 'True' }).Count`

type windowsGuard struct{}

func NewSystemGuard() DNSGuard { return windowsGuard{} }

func (windowsGuard) Apply(ctx context.Context, ifaces []string) error {
	if len(ifaces) == 0 {
		return errNoInterfaces
	}
	b, err := json.Marshal(ifaces)
	if err != nil {
		return err
	}
	_, err = runPS(ctx, psGuardApply, "KS_GUARD="+string(b))
	return err
}

func (windowsGuard) Remove(ctx context.Context) error {
	_, err := runPS(ctx, psGuardRemove)
	return err
}

func (windowsGuard) Active(ctx context.Context) (bool, error) {
	out, err := runPS(ctx, psGuardActive)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "2", nil
}
