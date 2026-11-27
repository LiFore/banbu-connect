#!/bin/bash
# 检查并强制退出 Banbu Connect 的所有后台组件（macOS）。
# 用法：sudo ./scripts/force-stop-banbu.sh [--check]
#   --check  只检查，不做任何改动（不需要 sudo，但部分 pf 信息需要 root 才能看到）
# 会做的事：卸载并删除 LaunchDaemon com.banbu.connect（以及改名前的 com.boxctl.desktop / com.boxctl.gate）的 plist，
# 结束托盘 App 和内核进程，撤销服务写入的保护规则（pf 锚点 boxguard / boxguard-dns、/etc/resolver 中带标记的文件）。
# 不会做的事：不删除程序文件和 /Library/Application Support 中的数据，不动系统代理设置。
# 之后要恢复：打开 Banbu Connect，在安装页重新安装后台服务。
set -u

LABELS=(com.banbu.connect com.boxctl.desktop com.boxctl.gate)
PATTERN='banbu[- ]connect|/usr/local/libexec/boxctl'
PF_ANCHOR=com.apple/boxguard
PF_PLIST=/Library/LaunchDaemons/com.boxguard.pf.plist
PF_TOKEN=/var/run/boxguard.pf-token
GUARD_ANCHOR=com.apple/boxguard-dns
GUARD_TOKEN=/var/run/boxguard-dns.pf-token
GUARD_CONF=/var/run/boxguard-dns.conf

tagged_resolvers() {
  for f in /etc/resolver/*; do
    [ -f "$f" ] && [ "$(head -n1 "$f" 2>/dev/null)" = "# boxguard" ] && echo "$f"
  done
}

show_state() {
  echo '--- LaunchDaemon ---'
  for l in "${LABELS[@]}"; do
    if launchctl print "system/$l" >/dev/null 2>&1; then
      echo "$l 已加载 $(launchctl print "system/$l" 2>/dev/null | grep -m1 -E '^\s*state =' | xargs)"
    elif [ -f "/Library/LaunchDaemons/$l.plist" ]; then
      echo "$l 未加载（plist 存在）"
    fi
  done
  echo '--- 进程 ---'
  pgrep -ilf "$PATTERN" || echo '(无)'
  echo '--- TUN / 默认路由 ---'
  route -n get default 2>/dev/null | grep -E 'gateway|interface'
  ifconfig 2>/dev/null | awk '/^utun/{i=$1} /inet 198\.18\./{print "TUN 地址：" i " " $2}'
  echo '--- 保护规则 ---'
  echo "pf 锚点 $PF_ANCHOR：$(pfctl -a "$PF_ANCHOR" -s rules 2>/dev/null | wc -l | xargs) 条"
  echo "pf 锚点 $GUARD_ANCHOR：$(pfctl -a "$GUARD_ANCHOR" -s rules 2>/dev/null | wc -l | xargs) 条"
  [ -f "$PF_PLIST" ] && echo "pf 加载项：$PF_PLIST"
  local r; r=$(tagged_resolvers); echo "带 boxguard 标记的 resolver：${r:-(无)}"
  echo '--- 系统代理（仅报告）---'
  scutil --proxy 2>/dev/null | grep -E 'Enable|Proxy |Port|PACURL' | xargs -L1 echo ' '
}

echo '===== 当前状态 ====='
show_state
[ "${1:-}" = "--check" ] && exit 0

if [ "$(id -u)" -ne 0 ]; then
  echo '需要管理员权限：请用 sudo 运行。' >&2
  exit 1
fi

echo; echo '===== 卸载 LaunchDaemon ====='
# KeepAlive 会立即重新拉起被结束的进程，所以必须先从 launchd 卸载，再结束进程。
for l in "${LABELS[@]}"; do
  if launchctl print "system/$l" >/dev/null 2>&1; then
    echo "bootout $l"
    launchctl bootout "system/$l" 2>/dev/null || true
  fi
done

echo; echo '===== 结束进程 ====='
osascript -e 'tell application "Banbu Connect" to quit' >/dev/null 2>&1 || true
for i in 1 2 3; do
  pgrep -if "$PATTERN" >/dev/null || break
  pgrep -ilf "$PATTERN" | sed 's/^/结束 /'
  pkill -9 -if "$PATTERN"
  sleep 2
done

echo; echo '===== 删除 LaunchDaemon ====='
for l in "${LABELS[@]}"; do
  p="/Library/LaunchDaemons/$l.plist"
  [ -f "$p" ] && { echo "删除 $p"; rm -f "$p"; }
done

echo; echo '===== 撤销保护规则 ====='
# 与 killswitch 的 darwinBackend.Remove / darwinGuard.Remove 相同；进程被强制结束时这些规则会残留。
launchctl bootout system "$PF_PLIST" 2>/dev/null || true
rm -f "$PF_PLIST"
pfctl -a "$PF_ANCHOR" -F all 2>/dev/null || true
[ -f "$PF_TOKEN" ] && { pfctl -X "$(cat "$PF_TOKEN")" 2>/dev/null; rm -f "$PF_TOKEN"; }
rm -rf /etc/boxguard
pfctl -a "$GUARD_ANCHOR" -F all 2>/dev/null || true
[ -f "$GUARD_TOKEN" ] && { pfctl -X "$(cat "$GUARD_TOKEN")" 2>/dev/null; rm -f "$GUARD_TOKEN"; }
rm -f "$GUARD_CONF"
tagged_resolvers | while read -r f; do echo "删除 $f"; rm -f "$f"; done
dscacheutil -flushcache
killall -HUP mDNSResponder 2>/dev/null || true

echo; echo '===== 处理后状态 ====='
show_state
if pgrep -if "$PATTERN" >/dev/null; then
  echo; echo '仍有进程在运行，见上方列表。' >&2
  exit 2
fi
echo; echo '完成。LaunchDaemon 已删除，开机不会再启动；程序文件和数据未删除。'
