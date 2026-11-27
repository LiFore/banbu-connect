#!/bin/bash
# 探测 macOS /etc/resolver：本地 DNS（Mihomo）消失后，域名解析是否失败而不是回退到系统 DNS。
# 用法：sudo bash probe-resolver-macos.sh /path/to/mihomo
#
# 只影响 example.net，持续约半分钟；退出时自动撤销。
set -u

MIHOMO="${1:-}"
DOMAIN="example.net"
PORT=1053
RESOLVER="/etc/resolver/$DOMAIN"

if [ "$(id -u)" -ne 0 ] || [ ! -x "$MIHOMO" ]; then
  echo "用法：sudo bash $0 /path/to/mihomo" >&2
  exit 1
fi
if [ -e "$RESOLVER" ]; then
  echo "$RESOLVER 已存在，为避免覆盖，退出" >&2
  exit 1
fi

DIR=$(mktemp -d)
CORE=""
cleanup() {
  [ -n "$CORE" ] && kill -9 "$CORE" 2>/dev/null
  rm -f "$RESOLVER"
  rm -rf "$DIR"
  flush
}
flush() {
  dscacheutil -flushcache
  killall -HUP mDNSResponder 2>/dev/null
  sleep 2
}
show() {
  flush
  echo "== $1"
  echo "  dscacheutil : $(dscacheutil -q host -a name "$DOMAIN" | awk '/ip_address/ {print $2}' | tr '\n' ',')"
  echo "  curl        : $(curl -s -m 6 -o /dev/null -w 'http=%{http_code} ip=%{remote_ip}' "http://$DOMAIN/")"
}
trap cleanup EXIT

cat > "$DIR/config.yaml" <<EOF
mixed-port: 0
log-level: warning
dns:
  enable: true
  listen: 127.0.0.1:$PORT
  enhanced-mode: fake-ip
  fake-ip-range: 198.18.0.1/16
  default-nameserver: [223.5.5.5]
  nameserver: [https://1.1.1.1/dns-query]
EOF

show "1. 基线（无规则，期望真实 IP）"

"$MIHOMO" -d "$DIR" -f "$DIR/config.yaml" >/dev/null 2>&1 &
CORE=$!
sleep 3
mkdir -p /etc/resolver
printf 'nameserver 127.0.0.1\nport %s\n' "$PORT" > "$RESOLVER"
show "2. 规则生效，内核运行（期望 198.18.x.x）"

kill -9 "$CORE"
CORE=""
sleep 2
show "3. 规则生效，内核被杀（全部为空或失败 = 故障关闭；出现真实 IP = 回退）"

rm -f "$RESOLVER"
show "4. 撤销后（期望真实 IP）"
