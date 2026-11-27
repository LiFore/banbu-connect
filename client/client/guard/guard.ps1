# Kill Switch for specific services (Windows). Nothing keeps running: the rules are persistent OS config.
#   DNS layer: NRPT sends protected suffixes only to Mihomo; if Mihomo is down, resolution fails.
#   IP layer:  Windows Firewall blocks the fixed CIDRs on physical adapters; TUN traffic is unaffected.
#
#   guard.ps1 plan                 show what would be applied (no admin needed)
#   guard.ps1 status               show what is installed (no admin needed)
#   guard.ps1 apply [-Force]       install (elevated); refuses unless Mihomo answers and TUN carries the traffic
#   guard.ps1 remove               uninstall (elevated); always works, even with Mihomo down
param(
    [Parameter(Mandatory = $true, Position = 0)][ValidateSet("plan", "status", "apply", "remove")][string]$Action,
    [string]$Listen = "127.0.0.2",
    [switch]$Force
)
$ErrorActionPreference = "Stop"

# TODO(verify:V4) apply and remove have never run on a real machine; see client/README.md.
# TODO(verify:V5) The firewall rule has no effect on a profile whose firewall is off (Private is off here).
# TODO(verify:V8) Assumes Clash Verge keeps dns.listen and Mihomo can bind 127.0.0.2:53 under Verge.

$Tag = "boxguard"
$Policy = Get-Content -Raw -Encoding UTF8 (Join-Path $PSScriptRoot "protected.json") | ConvertFrom-Json

function Get-Namespaces { foreach ($s in $Policy.suffixes) { $s; ".$s" } }
function Get-PhysicalAliases { @(Get-NetAdapter -Physical | ForEach-Object { $_.Name }) }
function Get-GuardNrpt { @(Get-DnsClientNrptRule | Where-Object { $_.Comment -eq $Tag }) }
function Get-GuardFirewall { @(Get-NetFirewallRule -Group $Tag -ErrorAction SilentlyContinue) }

function Assert-Elevated {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    if (-not (New-Object Security.Principal.WindowsPrincipal($id)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw "Run this from an elevated PowerShell."
    }
}

# Applying without a working path through Mihomo would cut these services off, including this terminal's Claude session.
function Assert-Ready {
    $probe = $Policy.suffixes[0]
    try {
        $ip = (Resolve-DnsName $probe -Server $Listen -Type A -DnsOnly -QuickTimeout -ErrorAction Stop | Where-Object { $_.IPAddress }).IPAddress | Select-Object -First 1
    } catch {
        throw "Mihomo is not answering DNS on ${Listen}:53. Check dns.listen in the script. (-Force skips this check)"
    }
    if ($ip -notlike "198.18.*") { throw "Expected a fake-ip for $probe from $Listen, got $ip. (-Force skips this check)" }

    $target = ($Policy.cidrs[0] -split "/")[0]
    $alias = (Find-NetRoute -RemoteIPAddress $target | Where-Object { $_.InterfaceAlias } | Select-Object -First 1).InterfaceAlias
    if ((Get-PhysicalAliases) -contains $alias) {
        throw "Traffic to $target still leaves through '$alias', so TUN is not active. (-Force skips this check)"
    }
}

function Remove-Guard {
    Get-GuardNrpt | ForEach-Object { Remove-DnsClientNrptRule -Name $_.Name -Force }
    Get-GuardFirewall | Remove-NetFirewallRule
    Clear-DnsClientCache
}

switch ($Action) {
    "plan" {
        Write-Output "NRPT -> ${Listen}:"
        Get-Namespaces | ForEach-Object { Write-Output "  $_" }
        Write-Output "Firewall block on [$((Get-PhysicalAliases) -join ', ')]:"
        $Policy.cidrs | ForEach-Object { Write-Output "  $_" }
    }
    "status" {
        $nrpt = Get-GuardNrpt
        $fw = Get-GuardFirewall
        Write-Output "NRPT rules: $($nrpt.Count)  servers: $(($nrpt | ForEach-Object { $_.NameServers } | Sort-Object -Unique) -join ',')"
        Write-Output "Firewall rules: $($fw.Count)  enabled: $(($fw | ForEach-Object { $_.Enabled } | Sort-Object -Unique) -join ',')"
        $off = @(Get-NetFirewallProfile | Where-Object { -not $_.Enabled } | ForEach-Object { $_.Name })
        if ($off.Count -gt 0) { Write-Output "WARNING: firewall profile off, IP layer inactive there: $($off -join ', ')" }
        foreach ($alias in Get-PhysicalAliases) {
            $bound = @($fw | Get-NetFirewallInterfaceFilter | Where-Object { @($_.InterfaceAlias) -contains $alias })
            if ($fw.Count -gt 0 -and $bound.Count -eq 0) { Write-Output "WARNING: adapter not covered, run apply again: $alias" }
        }
    }
    "apply" {
        Assert-Elevated
        if (-not $Force) { Assert-Ready }
        Remove-Guard
        try {
            Add-DnsClientNrptRule -Namespace @(Get-Namespaces) -NameServers $Listen -Comment $Tag | Out-Null
            New-NetFirewallRule -DisplayName "$Tag protected CIDRs" -Group $Tag -Direction Outbound -Action Block `
                -Protocol Any -Profile Any -RemoteAddress $Policy.cidrs -InterfaceAlias (Get-PhysicalAliases) | Out-Null
        } catch {
            Remove-Guard
            throw
        }
        Clear-DnsClientCache
        Write-Output "Applied. To undo from an elevated PowerShell: $PSCommandPath remove"
    }
    "remove" {
        Assert-Elevated
        Remove-Guard
        Write-Output "Removed."
    }
}
