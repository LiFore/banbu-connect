# 检查并强制退出 Banbu Connect 的所有后台组件（Windows）。
# 用法（管理员 PowerShell）：powershell -ExecutionPolicy Bypass -File scripts\force-stop-banbu.ps1 [-CheckOnly]
#   -CheckOnly  只检查，不做任何改动
# 会做的事：停止并删除 BanbuConnect 服务（以及改名前的 boxctl / boxctl-gate 服务）、结束托盘和内核进程、
# 撤销服务写入的保护规则（NRPT 和 boxguard / boxguard-dns 防火墙规则组）。
# 不会做的事：不删除程序文件和 ProgramData 中的数据、不动系统代理设置。
# 之后要恢复：打开 Banbu Connect，在安装页重新安装后台服务。
param([switch]$CheckOnly)

$ErrorActionPreference = 'Continue'
$Services  = 'BanbuConnect', 'boxctl', 'boxctl-gate'
$ProcNames = 'banbu-connect', 'Banbu Connect'
$FwGroups  = 'boxguard', 'boxguard-dns'

function Get-BanbuProcs { @(Get-Process -Name $ProcNames -ErrorAction SilentlyContinue) }

function Show-State {
    Write-Host '--- 服务 ---'
    foreach ($n in $Services) {
        $s = Get-Service -Name $n -ErrorAction SilentlyContinue
        if ($s) { Write-Host ("{0,-14} {1,-8} 启动类型={2}" -f $n, $s.Status, $s.StartType) }
    }
    Write-Host '--- 进程 ---'
    $procs = Get-BanbuProcs
    if ($procs.Count -eq 0) { Write-Host '(无)' }
    foreach ($p in $procs) {
        $ci = Get-CimInstance Win32_Process -Filter "ProcessId=$($p.Id)" -ErrorAction SilentlyContinue
        $ports = @(Get-NetTCPConnection -State Listen -OwningProcess $p.Id -ErrorAction SilentlyContinue |
            ForEach-Object { "$($_.LocalAddress):$($_.LocalPort)" }) -join ' '
        Write-Host ("PID {0,-6} {1}  {2}" -f $p.Id, $p.ProcessName, $ci.CommandLine)
        if ($ports) { Write-Host "           监听：$ports" }
    }
    Write-Host '--- TUN 网卡 / 默认路由 ---'
    Get-NetRoute -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue |
        ForEach-Object { Write-Host ("{0,-20} 下一跳={1,-15} 跃点={2}" -f $_.InterfaceAlias, $_.NextHop, $_.RouteMetric) }
    Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.IPAddress -like '198.18.*' } |
        ForEach-Object { Write-Host "TUN 地址：$($_.InterfaceAlias) $($_.IPAddress)" }
    Write-Host '--- 保护规则 ---'
    foreach ($g in $FwGroups) {
        $c = @(Get-NetFirewallRule -Group $g -ErrorAction SilentlyContinue).Count
        Write-Host "防火墙规则组 ${g}：$c 条"
    }
    $nrpt = @(Get-DnsClientNrptRule -ErrorAction SilentlyContinue | Where-Object { $_.Comment -eq 'boxguard' })
    Write-Host "NRPT 规则（boxguard）：$($nrpt.Count) 条"
    Write-Host '--- 系统代理（仅报告）---'
    $ie = Get-ItemProperty 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Internet Settings' -ErrorAction SilentlyContinue
    Write-Host "ProxyEnable=$($ie.ProxyEnable) ProxyServer=$($ie.ProxyServer) AutoConfigURL=$($ie.AutoConfigURL)"
}

Write-Host '===== 当前状态 ====='
Show-State
if ($CheckOnly) { exit 0 }

$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
    [Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $admin) { Write-Host '需要管理员权限：请用“以管理员身份运行”的 PowerShell 重新执行。' -ForegroundColor Red; exit 1 }

Write-Host "`n===== 停止服务 ====="
# 正常停止不会触发服务的恢复动作；直接结束服务进程会被当成崩溃，5 秒后被重新拉起，所以先走 SCM 停止。
foreach ($n in $Services) {
    $s = Get-Service -Name $n -ErrorAction SilentlyContinue
    if ($s -and $s.Status -ne 'Stopped') {
        Write-Host "停止 $n ..."
        Stop-Service -Name $n -Force -NoWait -ErrorAction SilentlyContinue
        try { $s.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(20)) } catch { Write-Host "  $n 20 秒内没有停下，下面强制结束进程" -ForegroundColor Yellow }
    }
}

Write-Host "`n===== 结束进程 ====="
# 被强制结束的服务进程可能被恢复动作拉起，所以多轮检查。
for ($i = 1; $i -le 3; $i++) {
    $procs = Get-BanbuProcs
    if ($procs.Count -eq 0) { break }
    foreach ($p in $procs) {
        Write-Host "结束 PID $($p.Id) $($p.ProcessName)"
        Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
    }
    Start-Sleep -Seconds 6
}

Write-Host "`n===== 删除服务 ====="
foreach ($n in $Services) {
    if (Get-Service -Name $n -ErrorAction SilentlyContinue) {
        Write-Host "删除 $n"
        sc.exe delete $n | Out-Null
        if ($LASTEXITCODE -ne 0) { Write-Host "  删除 $n 失败（sc.exe 返回 $LASTEXITCODE）" -ForegroundColor Red }
    }
}

Write-Host "`n===== 撤销保护规则 ====="
# 与 killswitch 的 psRemove / psGuardRemove 相同；服务被强制结束时这些规则会残留，导致 DNS 和受保护网段不通。
Get-DnsClientNrptRule -ErrorAction SilentlyContinue | Where-Object { $_.Comment -eq 'boxguard' } |
    ForEach-Object { Remove-DnsClientNrptRule -Name $_.Name -Force }
foreach ($g in $FwGroups) { Get-NetFirewallRule -Group $g -ErrorAction SilentlyContinue | Remove-NetFirewallRule }
Clear-DnsClientCache

Write-Host "`n===== 处理后状态 ====="
Show-State
$left = @($Services | Where-Object { Get-Service -Name $_ -ErrorAction SilentlyContinue })
if ((Get-BanbuProcs).Count -gt 0 -or $left.Count -gt 0) { Write-Host "`n仍有进程或服务残留，见上方列表（服务标记删除后可能要重启才会消失）。" -ForegroundColor Red; exit 2 }
Write-Host "`n完成。服务已删除，开机不会再启动；程序文件和数据未删除。" -ForegroundColor Green
