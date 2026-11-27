# Probe: does NRPT fail closed when the local DNS listener (Mihomo) is gone?
# Run from an elevated PowerShell:
#   powershell -ExecutionPolicy Bypass -File probe-nrpt.ps1 -Mihomo C:\path\to\mihomo.exe
#
# Only example.net is affected, for about a minute. The NRPT rule is always removed on exit.
# Do not run it while Clash Verge is using 127.0.0.2:53.
param(
    [Parameter(Mandatory = $true)][string]$Mihomo,
    [string]$Domain = "example.net",
    [string]$Listen = "127.0.0.2"
)

$id = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not (New-Object Security.Principal.WindowsPrincipal($id)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "Run this from an elevated PowerShell."
}

function Show-Resolution([string]$Label) {
    Clear-DnsClientCache
    Write-Output "== $Label"
    try {
        $a = (Resolve-DnsName $Domain -Type A -ErrorAction Stop | Where-Object { $_.IPAddress }).IPAddress -join ","
        Write-Output "  Resolve-DnsName : $a"
    } catch { Write-Output "  Resolve-DnsName : FAILED ($($_.Exception.Message))" }
    try {
        $a = ([System.Net.Dns]::GetHostAddresses($Domain) | ForEach-Object { $_.ToString() }) -join ","
        Write-Output "  getaddrinfo     : $a"
    } catch { Write-Output "  getaddrinfo     : FAILED ($($_.Exception.InnerException.Message))" }
    if (Get-Command node -ErrorAction SilentlyContinue) {
        $js = "require('dns').lookup('$Domain',{all:true},(e,r)=>console.log(e?'FAILED ('+e.code+')':r.map(x=>x.address).join(',')))"
        Write-Output "  node dns.lookup : $(node -e $js)"
    }
    $code = curl.exe -s -m 6 -o NUL -w "http=%{http_code} ip=%{remote_ip}" "http://$Domain/"
    Write-Output "  curl            : $code"
}

$dir = Join-Path $env:TEMP ("nrpt-probe-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $dir | Out-Null
$cfg = Join-Path $dir "config.yaml"
@"
mixed-port: 0
log-level: warning
dns:
  enable: true
  listen: ${Listen}:53
  enhanced-mode: fake-ip
  fake-ip-range: 198.18.0.1/16
  default-nameserver: [223.5.5.5]
  nameserver: [https://1.1.1.1/dns-query]
"@ | Out-File -FilePath $cfg -Encoding ascii

$core = $null
$rule = $null
try {
    Show-Resolution "1. baseline (no rule, expect real IPs)"

    $core = Start-Process -FilePath $Mihomo -ArgumentList @("-d", $dir, "-f", $cfg) -PassThru -WindowStyle Hidden
    Start-Sleep -Seconds 3
    $rule = Add-DnsClientNrptRule -Namespace ".$Domain", $Domain -NameServers $Listen -PassThru
    Show-Resolution "2. rule on, core up (expect 198.18.x.x)"

    Stop-Process -Id $core.Id -Force
    $core = $null
    Start-Sleep -Seconds 2
    Show-Resolution "3. rule on, core KILLED (fail-closed = all FAILED, a real IP = fallback)"
}
finally {
    if ($core) { Stop-Process -Id $core.Id -Force -ErrorAction SilentlyContinue }
    if ($rule) { Remove-DnsClientNrptRule -Name $rule.Name -Force }
    Clear-DnsClientCache
    Remove-Item -Recurse -Force $dir -ErrorAction SilentlyContinue
    Write-Output "== cleanup: NRPT rules left = $((Get-DnsClientNrptRule | Measure-Object).Count)"
}
Show-Resolution "4. after cleanup (expect real IPs)"
