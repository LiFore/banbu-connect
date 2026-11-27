'use strict';
'require view';
'require uci';
'require rpc';
'require fs';
'require dom';
'require poll';
'require ui';

// Banbu Box 的面板是独立的 HTTPS 页面（自签名证书），不能嵌进 LuCI 的 iframe。这里显示所有监控项：
// 管理面板、Mihomo 内核（独立服务，ADR-045）、保护服务 guard 各自的状态，guard 和面板每 0.5 秒对内核的检查，
// 各层阻断、严格线路出口、网络监控、规则自检、拦截计数和运行数据。
// 数据来自 `boxctl status`（面板、guard 的本机 unix socket 和内核的 pid 文件，各自独立读取）与 procd，每 2 秒刷新。

var callServiceList = rpc.declare({
	object: 'service',
	method: 'list',
	params: ['name'],
	expect: { '': {} }
});

var callBoard = rpc.declare({ object: 'system', method: 'board' });
var callInfo = rpc.declare({ object: 'system', method: 'info' });

var OK = '#15803d', WARN = '#b45309', BAD = '#c0271f', OFF = '#60666d';
var REFRESH = 2;

// ---- 格式化 ----

function procdInstance(list, name) {
	try {
		var inst = list[name].instances;
		for (var k in inst)
			return inst[k];
	} catch (e) {}
	return null;
}

function isSet(t) {
	return !!t && t.indexOf('0001-') !== 0;
}

function duration(s) {
	var d = Math.floor(s / 86400), h = Math.floor(s % 86400 / 3600), m = Math.floor(s % 3600 / 60);
	if (d > 0) return d + ' 天 ' + h + ' 小时';
	if (h > 0) return h + ' 小时 ' + m + ' 分';
	if (m > 0) return m + ' 分 ' + (s % 60) + ' 秒';
	return s + ' 秒';
}

function since(t) {
	if (!isSet(t)) return '-';
	return duration(Math.max(0, Math.floor((Date.now() - Date.parse(t)) / 1000)));
}

function ago(t) {
	if (!isSet(t)) return '-';
	var s = Math.max(0, (Date.now() - Date.parse(t)) / 1000);
	if (s < 1) return _('刚刚');
	if (s < 60) return Math.floor(s) + _(' 秒前');
	return duration(Math.floor(s)) + _('前');
}

function when(t) {
	if (!isSet(t)) return '-';
	return new Date(t).toLocaleString() + '（' + ago(t) + '）';
}

function bytes(n) {
	if (n == null || isNaN(n)) return '-';
	var u = ['B', 'KB', 'MB', 'GB', 'TB'], i = 0;
	while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
	return (i === 0 ? n : n.toFixed(n >= 100 ? 0 : 1)) + ' ' + u[i];
}

function num(n) {
	return n == null ? '-' : String(n);
}

// ---- 版式 ----

function badge(text, color) {
	return E('span', { 'style': 'display:inline-block;background:' + color + ';color:#fff;padding:1px 8px;border-radius:4px;font-size:12px;line-height:20px;white-space:nowrap' }, text);
}

function onoff(on, onText, offText, onColor) {
	return on ? badge(onText, onColor || WARN) : badge(offText, OFF);
}

// 版式都用内联样式，不依赖主题的 .table（主题表格居中、行高大、列等分，监控项会显得很松）。
var LINE = '1px solid rgba(128,128,128,.15)';
var PAD = 'box-sizing:border-box;padding:0 12px 12px';

// 键值行：标签列按内容宽度，值紧跟其后。
function rows(list) {
	var cells = [];
	list.filter(function(r) { return r; }).forEach(function(r) {
		cells.push(E('div', { 'style': 'padding:3px 0;border-bottom:' + LINE + ';opacity:.7;white-space:nowrap' }, r[0]));
		cells.push(E('div', { 'style': 'padding:3px 0;border-bottom:' + LINE + ';min-width:0;word-break:break-all' }, r[1]));
	});
	return E('div', { 'style': 'display:grid;grid-template-columns:max-content minmax(0,1fr);column-gap:20px;margin-top:6px;font-size:13px;line-height:20px' }, cells);
}

function card(title, state, body, basis) {
	return E('div', { 'style': 'box-sizing:border-box;flex:1 1 ' + (basis || '260px') + ';min-width:0;border:1px solid rgba(128,128,128,.25);border-radius:6px;padding:8px 12px 10px' }, [
		E('div', { 'style': 'display:flex;justify-content:space-between;align-items:center;gap:8px;min-height:22px' }, [
			E('strong', { 'style': 'font-size:13px' }, title), state || ''
		]),
		body
	]);
}

function tile(label, value, hint) {
	return E('div', { 'style': 'box-sizing:border-box;flex:1 1 130px;min-width:0;border:1px solid rgba(128,128,128,.25);border-radius:6px;padding:6px 12px' }, [
		E('div', { 'style': 'opacity:.7;font-size:12px' }, label),
		E('div', { 'style': 'font-size:18px;font-weight:600;line-height:26px;white-space:nowrap' }, value),
		hint ? E('div', { 'style': 'opacity:.6;font-size:11px' }, hint) : ''
	]);
}

function section(title, children, note) {
	return E('div', { 'class': 'cbi-section' }, [
		E('h3', {}, title),
		note ? E('div', { 'class': 'cbi-section-descr', 'style': 'margin-top:0;margin-bottom:8px;padding-top:0;padding-bottom:0' }, note) : '',
		E('div', { 'style': PAD + ';display:flex;flex-wrap:wrap;gap:10px' }, children)
	]);
}

// 数据表：左对齐、紧凑行高，窄屏时横向滚动而不是撑出页面。
function table(headers, body) {
	var cell = 'padding:4px 16px 4px 0;text-align:left;white-space:nowrap;border-bottom:' + LINE;
	return E('div', { 'style': 'overflow-x:auto;margin-top:8px' }, E('table', { 'style': 'width:100%;border-collapse:collapse;font-size:13px;line-height:20px' }, [
		E('tr', {}, headers.map(function(h) { return E('th', { 'style': cell + ';font-weight:600;opacity:.7' }, h); }))
	].concat(body.map(function(cells) {
		return E('tr', {}, cells.map(function(c) { return E('td', { 'style': cell }, c); }));
	}))));
}

function banner(color, title, text) {
	return E('div', { 'style': 'border-left:4px solid ' + color + ';background:rgba(128,128,128,.08);padding:10px 14px;border-radius:4px;margin-bottom:12px' }, [
		E('strong', { 'style': 'color:' + color }, title),
		text ? E('div', { 'style': 'margin-top:4px' }, text) : ''
	]);
}

function parseStatus(res) {
	try {
		return JSON.parse(res.stdout || '');
	} catch (e) {
		return { panel_error: _('无法读取状态：') + ((res && res.stderr) || e.message), guard_error: _('无法读取状态') };
	}
}

var GUARD_STATE = {
	healthy: [_('保护中'), OK],
	blocked: [_('阻断中'), WARN],
	init: [_('初始化'), OFF],
	error: [_('规则异常'), BAD]
};

var CONFIRM = {
	pending: _('正在确认出口'),
	confirmed: _('出口已确认'),
	unconfirmed: _('出口不通（上游故障，未回滚）'),
	rolled_back: _('已回滚到上一版'),
	failed: _('确认失败')
};

var COUNTERS = [
	['forward', _('局域网直连受保护服务'), _('forward 链')],
	['output', _('路由盒本机直连受保护服务'), _('output 链')],
	['hold', _('严格线路阻断'), _('hold 链')],
	['core_down', _('内核离线整体阻断'), _('core_down 链')],
	['encrypted_dns', _('加密 DNS'), _('DoT、DoQ、DoH')],
	['device_block', _('设备断网与时段管控'), _('device_block 链')]
];

return view.extend({
	load: function() {
		return Promise.all([
			uci.load('boxctl'),
			callBoard().catch(function() { return {}; })
		]);
	},

	fetch: function() {
		return Promise.all([
			fs.exec('/usr/bin/boxctl', ['status']).then(parseStatus).catch(function(e) {
				return { panel_error: e.message, guard_error: e.message };
			}),
			callServiceList('boxctl-panel').catch(function() { return {}; }),
			callServiceList('boxctl-core').catch(function() { return {}; }),
			callServiceList('boxctl-guard').catch(function() { return {}; }),
			callInfo().catch(function() { return {}; })
		]);
	},

	restart: function(service, warning) {
		return ui.showModal(_('确认重启'), [
			E('p', {}, warning),
			E('div', { 'class': 'right' }, [
				E('button', { 'class': 'btn', 'click': ui.hideModal }, _('取消')), ' ',
				E('button', {
					'class': 'btn cbi-button-negative',
					'click': ui.createHandlerFn(this, function() {
						return fs.exec('/etc/init.d/' + service, ['restart']).then(function() {
							ui.hideModal();
							ui.addNotification(null, E('p', {}, _('已发出重启命令，几秒后状态会刷新。')), 'info');
						}).catch(function(e) {
							ui.hideModal();
							ui.addNotification(null, E('p', {}, _('重启失败：') + e.message), 'danger');
						});
					})
				}, _('重启'))
			])
		]);
	},

	build: function(board, data) {
		var st = data[0] || {}, info = data[4] || {};
		var panelProc = procdInstance(data[1], 'boxctl-panel');
		var coreProc = procdInstance(data[2], 'boxctl-core');
		var guardProc = procdInstance(data[3], 'boxctl-guard');
		var p = st.panel, g = st.guard, rss = st.rss || {};
		var c = p ? p.core : null, svc = c ? c.service : null, gc = g ? g.core : null;
		var port = uci.get('boxctl', 'panel', 'https_port') || '8443';
		var url = 'https://' + window.location.hostname + ':' + port + '/';
		var corePID = st.core_pid || (coreProc && coreProc.pid) || 0;
		var coreRunning = !!(coreProc && coreProc.running) || corePID > 0;

		// ---- 保护总览 ----
		var overview;
		if (!g)
			overview = banner(BAD, _('保护服务 guard 没有运行'), _('已装上的保护规则仍在 nft 表里，但没有人自检和检查内核。') + (st.guard_error ? ' ' + st.guard_error : ''));
		else if (gc && gc.blocking)
			overview = banner(BAD, _('内核离线或异常：局域网已整体阻断'), _('只能访问路由盒和内网，内核恢复后（连续 2 次检查正常）自动解除。原因：') + (gc.reason || '-'));
		else if (g.hold)
			overview = banner(WARN, _('受保护的服务已阻断'), g.reason || '-');
		else if (p && p.killswitch.enabled)
			overview = banner(OK, _('受保护的服务正常'), _('只经严格线路访问，出口符合要求。'));
		else if (p)
			overview = banner(OFF, _('Kill Switch 没有开启'), _('受保护的服务不做阻断。'));
		else
			overview = banner(WARN, _('面板没有运行'), (st.panel_error || '') + _('；内核和 guard 不受影响，继续按上一份配置运行。'));

		// ---- 三个进程 ----
		var panelState = p ? badge(_('运行中'), OK) : (panelProc && panelProc.running ? badge(_('无响应'), WARN) : badge(_('未运行'), BAD));
		var panelCard = card(_('管理面板'), panelState, rows([
			[_('版本'), p ? p.panel.version : '-'],
			[_('PID'), p ? num(p.panel.pid) : (panelProc && panelProc.pid ? num(panelProc.pid) : '-')],
			[_('运行时长'), p ? since(p.panel.started_at) : '-'],
			[_('内存'), rss.panel ? bytes(rss.panel) : '-'],
			[_('地址'), E('a', { 'href': url, 'target': '_blank', 'rel': 'noopener' }, url)],
			[_('管理服务'), p ? (p.server_bound ? _('已绑定') : _('未绑定')) : '-'],
			[_('订阅'), p ? num(p.subscriptions) : '-'],
			!p && st.panel_error ? [_('错误'), st.panel_error] : null
		]));

		var coreState;
		if (gc && gc.expected && !gc.healthy) coreState = badge(_('异常'), BAD);
		else if (coreRunning && (!gc || gc.healthy)) coreState = badge(c && c.applying ? _('应用配置中') : _('运行中'), OK);
		else if (gc && !gc.expected) coreState = badge(_('已停用'), OFF);
		else coreState = badge(_('未运行'), BAD);
		var tun = '-';
		if (c) tun = c.tun ? (c.running ? (c.tun_active ? _('已开启') : _('未生效')) : _('已开启')) : _('关闭');
		var coreVersion = (svc && svc.version) || (gc && gc.version) || '-';
		var coreCard = card(_('Mihomo 内核（独立服务）'), coreState, rows([
			[_('托管方式'), svc ? (svc.managed === 'service' ? _('procd 服务 boxctl-core（面板重启不影响）') : _('面板子进程')) : (coreProc ? _('procd 服务 boxctl-core') : '-')],
			[_('版本'), svc && svc.needs_restart ? E('span', {}, [coreVersion, ' ', badge(_('安装包里是 ') + svc.bundled + _('，重启内核后生效'), WARN)]) : coreVersion],
			[_('PID'), corePID ? num(corePID) : '-'],
			[_('运行时长'), svc && svc.online ? since(svc.started_at) : '-'],
			[_('内存'), rss.core ? bytes(rss.core) : '-'],
			[_('模式'), c ? ({ rule: _('规则'), global: _('全局'), direct: _('直连') }[c.mode] || c.mode) : '-'],
			[_('TUN'), tun],
			[_('节点'), c ? num(c.node_count) : '-'],
			[_('配置版本'), p && p.config_version ? 'v' + p.config_version : '-'],
			[_('应用时间'), c && isSet(c.applied_at) ? when(c.applied_at) : '-'],
			c && c.confirm ? [_('配置确认'), CONFIRM[c.confirm] || c.confirm] : null,
			[_('意外离线'), svc ? (svc.restarts ? _('%d 次，最近 %s').format(svc.restarts, when(svc.offline_at)) : _('没有')) : '-'],
			c && c.tun_error ? [_('TUN 错误'), c.tun_error] : null,
			c && c.last_error ? [_('最近错误'), c.last_error] : null
		]));

		var gs = g ? (GUARD_STATE[g.state] || [g.state, OFF]) : null;
		var guardState = g ? badge(gs[0], gs[1]) : (guardProc && guardProc.running ? badge(_('无响应'), WARN) : badge(_('未运行'), BAD));
		var guardCard = card(_('保护服务（guard）'), guardState, rows([
			[_('版本'), g ? g.version : '-'],
			[_('PID'), g ? num(g.pid) : (guardProc && guardProc.pid ? num(guardProc.pid) : '-')],
			[_('运行时长'), g ? since(g.started_at) : '-'],
			[_('内存'), rss.guard ? bytes(rss.guard) : '-'],
			[_('保护范围'), g ? _('%d 个域名、%d 个网段').format(g.domains, g.cidrs) : '-'],
			[_('局域网网桥'), g ? (g.lan_device || '-') : '-'],
			g && g.last_error ? [_('错误'), g.last_error] : null,
			!g && st.guard_error ? [_('错误'), st.guard_error] : null
		]));

		// ---- 内核健康监控 ----
		var guardMon = card(_('guard 检查（每 0.5 秒）'), gc ? (gc.healthy ? badge(_('正常'), OK) : badge(_('异常'), BAD)) : badge(_('不可用'), OFF), rows([
			[_('检查内容'), _('控制接口、TUN 网卡、DNS 监听')],
			[_('内核应在运行'), gc ? (gc.expected ? _('是') : _('否（用户停用，不阻断）')) : '-'],
			[_('最近检查'), gc ? ago(gc.checked_at) : '-'],
			[_('响应耗时'), gc ? gc.latency_ms + ' ms' : '-'],
			[_('当前状态持续'), gc ? since(gc.since) : '-'],
			[_('连续失败'), gc ? num(gc.failures) : '-'],
			[_('累计检查 / 失败'), gc ? gc.checks + ' / ' + gc.fail_total : '-'],
			[_('触发整体阻断'), gc ? (gc.blocks ? _('%d 次，最近 %s').format(gc.blocks, when(gc.blocked_at)) : _('没有')) : '-'],
			gc && gc.reason ? [_('原因'), gc.reason] : null
		]), '320px');
		var panelMon = card(_('面板检查（每 0.5 秒）'), svc ? (svc.online ? badge(_('在线'), OK) : badge(_('离线'), BAD)) : badge(_('不可用'), OFF), rows([
			[_('检查内容'), _('pid 文件、控制接口')],
			[_('最近检查'), svc ? ago(svc.checked_at) : '-'],
			[_('响应耗时'), svc ? svc.latency_ms + ' ms' : '-'],
			svc && svc.last_error ? [_('错误'), svc.last_error] : null
		]), '320px');

		// ---- 阻断层级 ----
		var push = p ? p.push : null;
		var synced = '-';
		if (g && p) synced = p.guard_policy && p.guard_policy === g.policy_version ? badge(_('已同步'), OK) : badge(_('未同步，等待下发'), WARN);
		var layers = card(_('各层阻断'), null, rows([
			[_('严格线路（面板判断）'), push ? (push.hold ? E('span', {}, [badge(_('阻断'), WARN), ' ', push.reason]) : badge(_('放行'), OK)) : '-'],
			[_('guard：严格集合（nft）'), g ? onoff(g.hold, _('阻断'), _('放行')) : '-'],
			[_('内核：拒绝副本规则'), c ? (c.running ? onoff(c.hold_rules, _('打开（阻断）'), _('关闭（放行）')) : _('内核没在运行')) : '-'],
			[_('guard：内核离线整体阻断'), gc ? onoff(gc.blocking, _('阻断局域网'), _('没有触发'), BAD) : '-'],
			[_('加密 DNS 拦截'), g ? onoff(g.encrypted_dns, _('拦截中'), _('关闭'), OK) : '-'],
			[_('策略下发'), push ? (isSet(push.at) ? ago(push.at) : '-') : '-'],
			[_('guard 策略'), g ? E('span', {}, [g.policy_version + ' ', synced]) : '-'],
			push && push.error ? [_('下发错误'), E('span', { 'style': 'color:' + BAD }, push.error + (push.retrying ? _('（正在重试）') : ''))] : null
		]), '100%');

		// ---- 严格线路出口 ----
		var strict = p ? p.strict : null;
		var groups = (strict && strict.groups) || [];
		var strictSection = section(_('严格线路出口'), [
			E('div', { 'style': 'flex:1 1 100%;min-width:0' }, [
				rows([
					[_('允许的出口'), strict ? (strict.required ? strict.allowed.join('、') : _('管理服务没有下发，只展示结果，不阻断')) : '-'],
					strict && strict.reason ? [_('当前结论'), (strict.pending ? _('正在探测：') : '') + strict.reason] : null
				]),
				groups.length ? table([_('线路'), _('出口'), _('位置'), _('耗时'), _('探测时间'), _('结果')], groups.map(function(gr) {
					var eg = gr.egress || {};
					return [
						gr.group,
						eg.ip || '-',
						[eg.country, eg.colo].filter(function(x) { return x; }).join(' · ') || '-',
						eg.latency_ms ? eg.latency_ms + ' ms' : '-',
						ago(eg.checked_at),
						gr.allowed ? badge(_('符合'), OK) : E('span', { 'style': 'color:' + WARN }, gr.reason || eg.error || _('不符合'))
					];
				})) : E('em', {}, _('没有严格线路（或内核没在运行）'))
			])
		], _('经每条严格线路的探测入口访问探测地址，看到的就是这条线路的实际出口。正常每 30 秒一次，阻断时每 10 秒一次。'));

		// ---- 网络监控、规则自检 ----
		var net = p ? p.network : null;
		var watch = section(_('网络监控与规则自检'), [
			card(_('网络监控（面板，每 1 秒）'), null, rows([
				[_('监控内容'), _('默认路由、WAN 地址')],
				[_('最近变化'), net && isSet(net.changed_at) ? when(net.changed_at) : _('面板启动后没有变化')],
				[_('变化次数'), net ? num(net.changes) : '-'],
				[_('当前'), net ? E('code', { 'style': 'font-size:11px' }, net.fingerprint || _('（读不到路由表）')) : '-'],
				[_('变化时'), _('严格线路先阻断，重新探测出口符合后放通')]
			]), '320px'),
			card(_('规则自检（guard）'), g ? (g.installed ? badge(_('规则已装'), OK) : badge(_('规则缺失'), BAD)) : null, rows([
				[_('自检'), g ? _('每 5 秒，已 %d 次，最近 %s').format(g.checks, ago(g.last_check)) : '-'],
				[_('表被删时立即恢复'), g ? (g.watching ? _('已启用（nft monitor）') : _('未启用')) : '-'],
				[_('严格集合存盘'), g ? (isSet(g.saved_at) ? _('%d 个地址，%s').format(g.saved_sets, ago(g.saved_at)) : _('还没有保存')) : '-'],
				[_('当前状态持续'), g ? since(g.since) : '-']
			]), '320px')
		]);

		// ---- 拦截计数 ----
		var counters = (g && g.counters) || {};
		var counterSection = section(_('拦截计数（包）'), COUNTERS.map(function(x) {
			return tile(x[1], g && g.counters ? num(counters[x[0]] || 0) : '-', x[2]);
		}), _('guard 启动或规则重装后从 0 开始。'));

		// ---- 运行数据 ----
		var live = c && c.running;
		var traffic = section(_('运行数据'), [
			tile(_('上传'), live ? bytes(c.up) + '/s' : '-'),
			tile(_('下载'), live ? bytes(c.down) + '/s' : '-'),
			tile(_('连接数'), live ? num(c.connections) : '-'),
			tile(_('活动设备'), live ? num(c.devices) : '-'),
			tile(_('累计上传'), live ? bytes(c.upload_total) : '-'),
			tile(_('累计下载'), live ? bytes(c.download_total) : '-')
		]);

		// ---- 基础信息 ----
		var mem = info.memory || {}, load = info.load || [];
		var sys = section(_('基础信息'), [
			E('div', { 'style': 'flex:1 1 100%;min-width:0' }, rows([
				[_('Kill Switch'), p ? (p.killswitch.enabled ? _('已开启，保护 %d 个服务').format(p.killswitch.services) : _('关闭')) : '-'],
				[_('主机名'), board.hostname || '-'],
				[_('型号'), board.model || '-'],
				[_('固件'), board.release ? (board.release.description || board.release.version) : '-'],
				[_('系统内核'), board.kernel || '-'],
				[_('系统运行时长'), info.uptime ? duration(info.uptime) : '-'],
				[_('负载'), load.length ? load.map(function(x) { return (x / 65536).toFixed(2); }).join(' / ') : '-'],
				[_('系统内存'), mem.total ? _('可用 %s / 共 %s').format(bytes(mem.available || mem.free), bytes(mem.total)) : '-']
			]))
		]);

		// ---- guard 事件 ----
		var events = (g && g.events) || [];
		var color = { warn: WARN, error: BAD };
		var evSection = section(_('保护服务最近事件'), [
			E('div', { 'style': 'flex:1 1 100%;min-width:0' }, events.length ? table([_('时间'), _('事件')], events.map(function(ev) {
				return [new Date(ev.time).toLocaleString(), E('span', { 'style': 'white-space:normal;' + (color[ev.level] ? 'color:' + color[ev.level] : '') }, ev.message)];
			})) : E('em', {}, _('没有事件')))
		]);

		return E('div', {}, [
			overview,
			section(_('运行状态'), [panelCard, coreCard, guardCard]),
			section(_('内核健康监控'), [guardMon, panelMon],
				_('guard 不依赖面板：内核应在运行而检查失败时第一次就阻断，连续 2 次正常才解除。')),
			section(_('阻断状态'), [layers]),
			strictSection, watch, counterSection, traffic, sys, evSection
		]);
	},

	render: function(data) {
		var board = data[1] || {};
		var self = this;
		var port = uci.get('boxctl', 'panel', 'https_port') || '8443';
		var url = 'https://' + window.location.hostname + ':' + port + '/';
		var body = E('div', {}, E('p', { 'class': 'spinning' }, _('正在读取状态…')));

		var refresh = function() {
			return self.fetch().then(function(res) {
				dom.content(body, self.build(board, res));
			});
		};
		poll.add(refresh, REFRESH);
		refresh();

		var btn = function(label, cls, fn) {
			return E('button', { 'class': 'btn cbi-button ' + cls, 'click': fn }, label);
		};

		return E('div', { 'class': 'cbi-map' }, [
			E('h2', {}, _('Banbu Box')),
			E('div', { 'class': 'cbi-map-descr' },
				_('路由盒的代理、线路和保护服务在独立的面板里管理。首次打开时浏览器会提示证书不受信任（自签名），继续访问即可；初始密码在 /etc/boxctl/initial-password。')),
			E('div', { 'class': 'cbi-page-actions', 'style': 'text-align:left;margin-bottom:12px' }, [
				btn(_('打开 Banbu Box 面板'), 'cbi-button-apply', function() { window.open(url, '_blank', 'noopener'); }), ' ',
				btn(_('重启面板'), 'cbi-button-action', function() {
					self.restart('boxctl-panel', _('只重启管理面板：内核和 guard 不受影响，连接不断。面板启动后会重新确认严格线路的出口，期间受保护的服务可能短暂阻断。'));
				}), ' ',
				btn(_('重启内核'), 'cbi-button-action', function() {
					self.restart('boxctl-core', _('重启 Mihomo 内核会断开所有连接；内核离线期间 guard 会阻断局域网（开启 Kill Switch 时），内核恢复后自动解除。安装包更新了内核版本时，重启后才会用上新版本。'));
				}), ' ',
				btn(_('重启 guard'), 'cbi-button-action', function() {
					self.restart('boxctl-guard', _('重启 guard 时会重新装上保护规则，已有规则保持不变，通常不影响上网。'));
				})
			]),
			body
		]);
	},

	handleSaveApply: null,
	handleSave: null,
	handleReset: null
});
