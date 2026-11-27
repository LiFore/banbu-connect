#!/bin/sh
# 故障注入（M13-03 / M9-10）：在 iStoreOS 路由盒上逐项制造故障，同时在 WAN 口抓包，
# 统计发往受保护服务地址（guard 严格集合）的包。任何一个都算泄漏：正常情况下这些地址只能经内核的代理出去，
# WAN 口上只应看到发往代理节点的流量。
#
# 用法（在路由盒上以 root 运行）：
#   sh fault-inject.sh [-c user@client] [-w wan网卡] [-f 翻转分钟数] [场景...]
#
# 场景：baseline core-restart core-crash panel-restart guard-restart fw4-reload fw4-restart
#       table-delete flapping；不写时依次跑全部。冷启动单独两步：
#   sh fault-inject.sh boot          装一次性抓包脚本后重启
#   sh fault-inject.sh boot-report   重启后查看冷启动期间的结果
#
# 测试期间要有局域网客户端持续访问受保护服务（含自定义 DNS、DoH、QUIC），见 scripts/fault-traffic.sh：
#   -c user@client 时本脚本经 ssh 在客户端上启动它；否则请先在客户端上手动运行。
#
# 依赖：tcpdump（opkg install tcpdump-mini）、nft。结果写到 /tmp/fault-report.txt。
set -u

REPORT=/tmp/fault-report.txt
WORK=/tmp/fault-inject
CLIENT=""
WAN=""
FLAP_MIN=10
SETTLE=20 # 每个故障之后继续抓包的秒数
TABLE="inet proxy_guard"
BOOT_INIT=/etc/init.d/boxctl-faultboot
BOOT_DIR=/etc/boxctl/faultboot

log() { echo "[$(date +%H:%M:%S)] $*"; }

die() {
	echo "错误：$*" >&2
	exit 1
}

while getopts "c:w:f:" opt; do
	case "$opt" in
	c) CLIENT="$OPTARG" ;;
	w) WAN="$OPTARG" ;;
	f) FLAP_MIN="$OPTARG" ;;
	*) die "未知参数" ;;
	esac
done
shift $((OPTIND - 1))

[ "$(id -u)" = 0 ] || die "需要 root"
command -v nft >/dev/null || die "没有 nft"

wan_dev() {
	[ -n "$WAN" ] && {
		echo "$WAN"
		return
	}
	d="$(uci -q get network.wan.device)"
	[ -z "$d" ] && d="$(uci -q get network.wan.ifname)"
	[ -z "$d" ] && d="$(ip route show default 2>/dev/null | awk '{for(i=1;i<NF;i++) if($i=="dev"){print $(i+1); exit}}')"
	echo "$d"
}

# strict_targets 输出严格集合里的地址和网段，每行一个。
strict_targets() {
	for s in strict_v4 strict_v4_net strict_v6 strict_v6_net; do
		nft list set $TABLE $s 2>/dev/null |
			tr -d '\n' | sed -n 's/.*elements = {\(.*\)}.*/\1/p' |
			tr ',' '\n' | sed 's/ timeout.*//; s/ expires.*//; s/^ *//; s/ *$//' | grep -v '^$'
	done
}

# bpf_filter 把严格集合拼成 tcpdump 过滤式：只看发出去的包（dst）。
bpf_filter() {
	f=""
	for t in $(strict_targets); do
		case "$t" in
		*-*) continue ;; # 自动合并出的地址区间，tcpdump 不支持；网段很少合并成区间
		*/*) e="dst net $t" ;;
		*) e="dst host $t" ;;
		esac
		f="${f:+$f or }$e"
	done
	echo "$f"
}

core_pid() {
	# 内核是 panel 的子进程：boxctl mihomo -watch-stdin ...
	pgrep -f "boxctl mihomo -watch-stdin" | head -n1
}

# capture 场景名 秒数 动作命令...：后台抓包，执行动作，等够秒数后统计泄漏包数。
capture() {
	name="$1"
	secs="$2"
	shift 2
	filter="$(bpf_filter)"
	if [ -z "$filter" ]; then
		result "$name" SKIP "严格集合为空：请先让客户端访问一次受保护服务"
		return
	fi
	pcap="$WORK/$name.pcap"
	rm -f "$pcap"
	tcpdump -i "$DEV" -n -U -w "$pcap" "$filter" >/dev/null 2>&1 &
	tp=$!
	sleep 1
	log "场景 $name：$*"
	"$@"
	sleep "$secs"
	kill "$tp" 2>/dev/null
	wait "$tp" 2>/dev/null
	n="$(tcpdump -n -r "$pcap" 2>/dev/null | wc -l | tr -d ' ')"
	if [ "${n:-0}" = 0 ]; then
		result "$name" PASS "WAN 口没有发往受保护地址的包"
	else
		result "$name" FAIL "WAN 口有 $n 个发往受保护地址的包，见 $pcap"
		tcpdump -n -r "$pcap" 2>/dev/null | head -n 5 | sed 's/^/    /' | tee -a "$REPORT"
	fi
}

result() {
	printf '%-14s %-4s %s\n' "$1" "$2" "$3" | tee -a "$REPORT"
}

guard_check() {
	# 自检：表在不在、版本注释在不在
	nft list chain $TABLE output 2>/dev/null | grep -q 'boxctl:'
}

# ---- 动作 ----

act_none() { :; }

act_core_restart() {
	p="$(core_pid)"
	[ -n "$p" ] || {
		log "内核没有运行"
		return
	}
	kill -TERM "$p"
}

act_core_crash() {
	p="$(core_pid)"
	[ -n "$p" ] || {
		log "内核没有运行"
		return
	}
	kill -KILL "$p"
}

act_panel_restart() { /etc/init.d/boxctl-panel restart; }
act_guard_restart() { /etc/init.d/boxctl-guard restart; }
act_fw4_reload() { fw4 reload >/dev/null 2>&1 || /etc/init.d/firewall reload; }
act_fw4_restart() { fw4 restart >/dev/null 2>&1 || /etc/init.d/firewall restart; }

act_table_delete() {
	nft delete table $TABLE
	i=0
	while [ $i -lt 10 ]; do
		sleep 1
		i=$((i + 1))
		if guard_check; then
			log "guard 在 ${i} 秒内恢复了表"
			[ $i -le 6 ] || log "警告：恢复超过 5 秒"
			return
		fi
	done
	log "警告：10 秒内表没有恢复"
}

act_flapping() {
	end=$(($(date +%s) + FLAP_MIN * 60))
	k=0
	while [ "$(date +%s)" -lt "$end" ]; do
		p="$(core_pid)"
		[ -n "$p" ] && kill -KILL "$p" && k=$((k + 1))
		sleep 10
	done
	log "翻转结束，共杀了 $k 次内核"
}

run_scenario() {
	case "$1" in
	baseline) capture baseline 60 act_none ;;
	core-restart) capture core-restart "$SETTLE" act_core_restart ;;
	core-crash) capture core-crash "$SETTLE" act_core_crash ;;
	panel-restart) capture panel-restart 40 act_panel_restart ;;
	guard-restart) capture guard-restart "$SETTLE" act_guard_restart ;;
	fw4-reload) capture fw4-reload "$SETTLE" act_fw4_reload ;;
	fw4-restart) capture fw4-restart "$SETTLE" act_fw4_restart ;;
	table-delete) capture table-delete "$SETTLE" act_table_delete ;;
	flapping) capture flapping "$SETTLE" act_flapping ;;
	*) result "$1" SKIP "未知场景" ;;
	esac
}

# ---- 冷启动 ----

boot_install() {
	mkdir -p "$BOOT_DIR"
	# 重启后严格集合里动态解析的地址会清空，先记下来，冷启动期间按它们抓包。
	strict_targets >"$BOOT_DIR/targets"
	[ -s "$BOOT_DIR/targets" ] || die "严格集合为空：请先让客户端访问一次受保护服务"
	echo "$(wan_dev)" >"$BOOT_DIR/wan"
	cat >"$BOOT_INIT" <<'EOF'
#!/bin/sh /etc/rc.common
# fault-inject.sh 安装的一次性脚本：先于 guard(18) 启动，冷启动的前 180 秒在 WAN 口抓包。
START=17
start() {
	dir=/etc/boxctl/faultboot
	f=""
	for t in $(cat $dir/targets); do
		case "$t" in *-*) continue ;; */*) e="dst net $t" ;; *) e="dst host $t" ;; esac
		f="${f:+$f or }$e"
	done
	wan="$(cat $dir/wan)"
	(
		i=0
		while [ $i -lt 60 ] && ! ip link show "$wan" >/dev/null 2>&1; do sleep 1; i=$((i+1)); done
		date +%s >$dir/start
		timeout 180 tcpdump -i "$wan" -n -U -w $dir/boot.pcap "$f" >/dev/null 2>&1
		date +%s >$dir/done
	) &
	/etc/init.d/boxctl-faultboot disable
}
EOF
	chmod +x "$BOOT_INIT"
	"$BOOT_INIT" enable
	log "已安装一次性抓包脚本。客户端保持访问受保护服务，3 秒后重启；重启约 3 分钟后运行：sh $0 boot-report"
	sleep 3
	reboot
}

boot_report() {
	[ -f "$BOOT_DIR/done" ] || die "还没有结果：抓包持续 180 秒，或没有执行过 boot"
	n="$(tcpdump -n -r "$BOOT_DIR/boot.pcap" 2>/dev/null | wc -l | tr -d ' ')"
	if [ "${n:-0}" = 0 ]; then
		result cold-boot PASS "冷启动 180 秒内 WAN 口没有发往受保护地址的包"
	else
		result cold-boot FAIL "冷启动期间有 $n 个包，见 $BOOT_DIR/boot.pcap"
		tcpdump -n -tttt -r "$BOOT_DIR/boot.pcap" 2>/dev/null | head -n 5 | sed 's/^/    /' | tee -a "$REPORT"
	fi
	rm -f "$BOOT_INIT"
}

# ---- 主流程 ----

mkdir -p "$WORK"
case "${1:-}" in
boot)
	boot_install
	exit 0
	;;
boot-report)
	boot_report
	exit 0
	;;
esac

command -v tcpdump >/dev/null || die "没有 tcpdump：opkg update && opkg install tcpdump-mini"
DEV="$(wan_dev)"
[ -n "$DEV" ] && ip link show "$DEV" >/dev/null 2>&1 || die "找不到 WAN 网卡，用 -w 指定"
guard_check || die "guard 的规则不在：先启动 boxctl-guard"

scenarios="${*:-baseline core-restart core-crash panel-restart guard-restart fw4-reload fw4-restart table-delete flapping}"

{
	echo "# 故障注入报告 $(date '+%F %T')"
	echo "# WAN=$DEV 受保护地址 $(strict_targets | wc -l | tr -d ' ') 条 翻转 ${FLAP_MIN} 分钟"
} >"$REPORT"

if [ -n "$CLIENT" ]; then
	log "在 $CLIENT 上启动流量脚本"
	ssh "$CLIENT" 'cat >/tmp/fault-traffic.sh' <"$(dirname "$0")/fault-traffic.sh" &&
		ssh "$CLIENT" 'nohup bash /tmp/fault-traffic.sh >/tmp/fault-traffic.log 2>&1 </dev/null &' ||
		die "无法在客户端启动 fault-traffic.sh"
	sleep 15
else
	log "请确认局域网客户端上已在运行 fault-traffic.sh；15 秒后开始"
	sleep 15
fi

for s in $scenarios; do
	run_scenario "$s"
done

if [ -n "$CLIENT" ]; then
	ssh "$CLIENT" 'pkill -f fault-traffic || true' >/dev/null 2>&1
fi

echo
log "报告：$REPORT"
if grep -q ' FAIL ' "$REPORT"; then
	echo "结果：不通过"
	exit 1
fi
echo "结果：通过（冷启动另用 boot / boot-report 两步验证）"
