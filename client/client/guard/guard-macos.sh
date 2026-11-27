#!/bin/bash
# 特定服务的 Kill Switch（macOS）。除开机加载一次 pf 外，没有常驻进程。
#   DNS 层：/etc/resolver 把受保护后缀只交给 Mihomo；Mihomo 不在时解析失败。
#   IP 层：pf 在物理网卡上拦截固定网段；走 TUN 的流量不受影响。
#
#   guard-macos.sh plan              查看将要应用的内容
#   sudo guard-macos.sh status       查看已安装的内容
#   sudo guard-macos.sh apply [--force]  安装；Mihomo DNS 或 TUN 未就绪时拒绝
#   sudo guard-macos.sh remove       卸载；Mihomo 不在时也能执行
#
# 需要在 Clash Verge 脚本里设置 DNS_LISTEN = "127.0.0.1:1053"。
set -eu

TAG="# boxguard"
LISTEN_IP="127.0.0.1"
LISTEN_PORT="1053"
ANCHOR="com.apple/boxguard"
CONF_DIR="/etc/boxguard"
RESOLVER_DIR="/etc/resolver"
PLIST="/Library/LaunchDaemons/com.boxguard.pf.plist"
TOKEN_FILE="/var/run/boxguard.pf-token"
POLICY="$(cd "$(dirname "$0")" && pwd)/protected.json"

# protected.json 的格式由本仓库控制：取指定数组里所有带引号的字符串。
policy_list() {
  awk -v key="\"$1\"" '
    index($0, key) { on = 1; next }
    on && /\]/ { exit }
    on { gsub(/[ ",\r]/, ""); if (length($0)) print }
  ' "$POLICY"
}

physical_interfaces() {
  networksetup -listallhardwareports | awk '/^Device: en[0-9]+/ {print $2}' | tr '\n' ' ' | sed 's/ *$//'
}

pf_rules() {
  local cidrs v4="" v6="" c
  cidrs=$(policy_list cidrs)
  for c in $cidrs; do
    case "$c" in
      *:*) v6="$v6 $c" ;;
      *) v4="$v4 $c" ;;
    esac
  done
  [ -z "$v4" ] || echo "block drop out quick on { $(physical_interfaces) } inet from any to {$v4 }"
  [ -z "$v6" ] || echo "block drop out quick on { $(physical_interfaces) } inet6 from any to {$v6 }"
}

require_root() {
  if [ "$(id -u)" -ne 0 ]; then
    echo "请用 sudo 执行" >&2
    exit 1
  fi
}

flush_dns() {
  dscacheutil -flushcache
  killall -HUP mDNSResponder 2>/dev/null || true
}

# 没有经 Mihomo 的通路就启用，会把这些服务（包括这台机器上的 Claude 会话）直接切断。
assert_ready() {
  local probe answer target iface
  probe=$(policy_list suffixes | head -1)
  answer=$(dig +short +time=2 +tries=1 "$probe" "@$LISTEN_IP" -p "$LISTEN_PORT" 2>/dev/null | head -1)
  case "$answer" in
    198.18.*) ;;
    *)
      echo "拒绝执行：$LISTEN_IP:$LISTEN_PORT 没有为 $probe 返回 fake-ip（得到：${answer:-无响应}）。检查脚本的 DNS_LISTEN。--force 可跳过" >&2
      exit 1
      ;;
  esac
  target=$(policy_list cidrs | head -1 | cut -d/ -f1)
  iface=$(route -n get "$target" 2>/dev/null | awk '/interface:/ {print $2}')
  case "$iface" in
    utun*) ;;
    *)
      echo "拒绝执行：访问 $target 走的是 ${iface:-未知接口}，TUN 没有接管流量。--force 可跳过" >&2
      exit 1
      ;;
  esac
}

# TODO(verify:V2) 实机上出现过 /etc/boxguard 不存在的情况，需确认是否只来自这里的 remove。
remove_guard() {
  local f
  launchctl bootout system "$PLIST" 2>/dev/null || true
  rm -f "$PLIST"
  pfctl -a "$ANCHOR" -F all >/dev/null 2>&1 || true
  if [ -f "$TOKEN_FILE" ]; then
    pfctl -X "$(cat "$TOKEN_FILE")" >/dev/null 2>&1 || true
    rm -f "$TOKEN_FILE"
  fi
  rm -rf "$CONF_DIR"
  if [ -d "$RESOLVER_DIR" ]; then
    for f in "$RESOLVER_DIR"/*; do
      if [ -f "$f" ] && grep -qxF "$TAG" "$f"; then rm -f "$f"; fi
    done
  fi
  flush_dns
}

apply_guard() {
  local s
  # 先检查再改动：不覆盖别的程序或用户写的 resolver 文件。
  for s in $(policy_list suffixes); do
    if [ -e "$RESOLVER_DIR/$s" ] && ! grep -qxF "$TAG" "$RESOLVER_DIR/$s"; then
      echo "拒绝执行：$RESOLVER_DIR/$s 已存在且不是本工具创建的" >&2
      exit 1
    fi
  done

  remove_guard
  trap 'remove_guard; echo "安装失败，已撤销" >&2' ERR

  mkdir -p "$RESOLVER_DIR" "$CONF_DIR"
  for s in $(policy_list suffixes); do
    printf '%s\nnameserver %s\nport %s\n' "$TAG" "$LISTEN_IP" "$LISTEN_PORT" > "$RESOLVER_DIR/$s"
  done

  # TODO(verify:V1) 规则已能加载，但还没有证明它真的拦截了流量。
  # TODO(verify:V3) 开机由下面的 LaunchDaemon 加载，重启后的效果还没有确认。
  pf_rules > "$CONF_DIR/pf.conf"
  cat > "$CONF_DIR/load-pf.sh" <<EOF
#!/bin/sh
/sbin/pfctl -E 2>&1 | /usr/bin/awk '/Token/ {print \$NF}' > "$TOKEN_FILE"
/sbin/pfctl -a "$ANCHOR" -f "$CONF_DIR/pf.conf"
EOF
  cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.boxguard.pf</string>
  <key>ProgramArguments</key>
  <array><string>/bin/sh</string><string>$CONF_DIR/load-pf.sh</string></array>
  <key>RunAtLoad</key><true/>
</dict>
</plist>
EOF
  launchctl bootstrap system "$PLIST"
  sleep 1
  if [ -z "$(pfctl -a "$ANCHOR" -s rules 2>/dev/null)" ]; then
    echo "pf 规则没有加载成功" >&2
    false
  fi
  trap - ERR
  flush_dns
  echo "已启用。撤销：sudo bash $0 remove"
}

show_status() {
  local installed=0 s iface
  for s in $(policy_list suffixes); do
    if [ -f "$RESOLVER_DIR/$s" ] && grep -qxF "$TAG" "$RESOLVER_DIR/$s"; then installed=$((installed + 1)); fi
  done
  echo "resolver 文件：$installed / $(policy_list suffixes | wc -l | tr -d ' ')"
  echo "pf 启用状态：$(pfctl -s info 2>/dev/null | awk '/^Status:/ {print $2}')"
  echo "pf 规则："
  pfctl -a "$ANCHOR" -s rules 2>/dev/null | sed 's/^/  /'
  if [ -f "$CONF_DIR/pf.conf" ]; then
    for iface in $(physical_interfaces); do
      grep -qw "$iface" "$CONF_DIR/pf.conf" || echo "警告：网卡 $iface 未覆盖，请重新 apply"
    done
  fi
}

case "${1:-}" in
  plan)
    echo "/etc/resolver -> $LISTEN_IP:$LISTEN_PORT"
    policy_list suffixes | sed 's/^/  /'
    echo "pf anchor $ANCHOR:"
    pf_rules | sed 's/^/  /'
    ;;
  status)
    require_root
    show_status
    ;;
  apply)
    require_root
    [ "${2:-}" = "--force" ] || assert_ready
    apply_guard
    ;;
  remove)
    require_root
    remove_guard
    echo "已移除"
    ;;
  *)
    echo "用法：$0 plan|status|apply [--force]|remove" >&2
    exit 2
    ;;
esac
