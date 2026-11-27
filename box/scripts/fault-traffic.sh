#!/usr/bin/env bash
# 故障注入的客户端流量（M9-10）：在局域网客户端（macOS / Linux）上循环访问受保护服务，
# 覆盖几种绕过方式，让路由盒上的 fault-inject.sh 在 WAN 口能观察到泄漏：
#   - 正常 HTTPS（系统 DNS → 路由盒 dnsmasq）
#   - 自定义 DNS：直接问 8.8.8.8 拿地址再连（guard 会把 53 重定向回路由盒）
#   - DoH：curl --doh-url 经 1.1.1.1 解析，绕过路由盒 DNS
#   - 缓存地址：用第一次解析到的 IP 直接连（模拟客户端缓存了解析结果、内核又不在）
#   - QUIC：curl --http3-only（curl 不支持时跳过）
#
# 用法：bash fault-traffic.sh [域名...]，默认 claude.ai chatgpt.com。Ctrl-C 结束。
# 连不上是预期的（被阻断）；本脚本只负责制造流量，泄漏由路由盒判断。
set -u

domains=("$@")
[ ${#domains[@]} -gt 0 ] || domains=(claude.ai chatgpt.com)

curl_q() { curl -s -o /dev/null -m 4 -w '%{http_code}' "$@" 2>/dev/null || true; }

has_http3=false
curl --help all 2>/dev/null | grep -q -- '--http3-only' && has_http3=true

# macOS 自带 bash 3.2 没有关联数组：cached[i] 对应 domains[i]。
cached=()
for d in "${domains[@]}"; do
	ip="$(dig +short "$d" A 2>/dev/null | grep -E '^[0-9.]+$' | head -n1)"
	cached+=("$ip")
	echo "$d 首次解析：${ip:-无}"
done

round=0
while true; do
	round=$((round + 1))
	line="#$round"
	for i in "${!domains[@]}"; do
		d="${domains[$i]}"
		line+=" $d:https=$(curl_q "https://$d/")"
		ip="$(dig +short @8.8.8.8 "$d" A 2>/dev/null | grep -E '^[0-9.]+$' | head -n1)"
		[ -n "$ip" ] && line+=" dns8=$(curl_q --resolve "$d:443:$ip" "https://$d/")"
		line+=" doh=$(curl_q --doh-url https://1.1.1.1/dns-query "https://$d/")"
		[ -n "${cached[$i]}" ] && line+=" cached=$(curl_q --resolve "$d:443:${cached[$i]}" "https://$d/")"
		$has_http3 && line+=" quic=$(curl_q --http3-only "https://$d/")"
	done
	echo "$(date +%H:%M:%S) $line"
	sleep 1
done
