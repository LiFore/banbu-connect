import { useCallback, useEffect, useState, type ReactNode } from 'react'
import { Alert, App, Badge, Button, Card, Skeleton, Switch, Tag, Tooltip } from 'antd'
import { Link } from 'react-router-dom'
import { CheckCircleFilled, CheckOutlined, ReloadOutlined, RightOutlined, SyncOutlined } from '@ant-design/icons'
import {
  api,
  ApiRequestError,
  type DesktopSummary,
  type KillSwitchState,
  type NetworkStatus,
  type ProxyMode,
  type RulesRefresh,
  type StrictGroup,
  type StrictStatus,
} from '../../api'
import CountryFlag from './CountryFlag'
import Accounts from './Accounts'
import { tunState } from './ProxySettings'
import { AppUpdateBanner, AppVersionLine, useAppUpdate } from './AppUpdate'

export type Tone = 'ok' | 'warn' | 'bad' | 'off'

export const ksStates: Record<KillSwitchState, { label: string; color: string; tone: Tone; desc: string }> = {
  off: { label: '未启用', color: 'default', tone: 'off', desc: '代理故障时受保护的服务可能直连' },
  protecting: { label: '保护中', color: 'green', tone: 'ok', desc: '内核故障时自动断开受保护的服务' },
  proxy_only: { label: '保护中，TUN 未接管', color: 'orange', tone: 'warn', desc: '只有走系统代理的应用能访问' },
  blocking: { label: '已断开受保护的服务', color: 'orange', tone: 'warn', desc: '内核不可用，打开代理后恢复' },
  direct: { label: '受信网络，直连', color: 'green', tone: 'ok', desc: '代理由路由盒负责' },
  held: { label: '已阻断', color: 'orange', tone: 'warn', desc: '出口未确认或不符合要求' },
  error: { label: '保护可能不完整', color: 'red', tone: 'bad', desc: '系统规则与配置不一致' },
}

export function networkLabel(n: NetworkStatus): string {
  const { network } = n
  if (!network.interface) return '无网络'
  if (network.ssid) return network.ssid
  if (network.wifi) return 'Wi-Fi'
  return network.interface
}

function egressLabel(n: NetworkStatus): string {
  const e = n.egress
  if (!n.probing) return '—'
  if (e.ok) return e.country ? `${e.ip}（${e.country}）` : e.ip
  if (e.error) return '探测失败'
  return '探测中'
}

function strictEgressLabel(g: StrictGroup): string {
  const e = g.egress
  if (e.ok) return e.country ? `${e.ip}（${e.country}）` : e.ip
  if (e.error) return '探测失败'
  return '探测中'
}

function strictTag(s: StrictStatus, g: StrictGroup): { label: string; color: string } {
  if (g.allowed) return s.required ? { label: '符合', color: 'green' } : { label: '可用', color: 'default' }
  if (!g.egress.ok) return { label: g.egress.error ? '失败' : '探测中', color: g.egress.error ? 'red' : 'default' }
  return { label: '不符合', color: 'red' }
}

export function StrictRows({ strict }: { strict: StrictStatus }) {
  return (
    <>
      {strict.groups.map((g) => {
        const tag = strictTag(strict, g)
        return (
          <div className="setting-row" key={g.group}>
            <span>{g.group}</span>
            <span className="setting-control selectable" title={g.reason ?? g.egress.error}>
              {strictEgressLabel(g)}
              <Tag color={tag.color}>{tag.label}</Tag>
            </span>
          </div>
        )
      })}
    </>
  )
}


const modeLabels: Record<ProxyMode, string> = { rule: '规则', global: '全局', direct: '直连' }

// 严格线路出口的一行摘要：逐组明细在「高级 · 出口检测」。
function strictSummary(s: StrictStatus): { label: string; color: string } {
  const bad = s.groups.filter((g) => !g.allowed)
  if (bad.length === 0) return s.required ? { label: '全部符合', color: 'green' } : { label: '可用', color: 'default' }
  if (bad.every((g) => !g.egress.ok && !g.egress.error)) return { label: '探测中', color: 'default' }
  return { label: `${bad.map((g) => g.group).join('、')} 不符合`, color: 'red' }
}

// 首页（BXC-F0-048，Q-013，BXC-F0-042）：概览和账号合在一页。上面是总体状态条（两个主开关）和一行要点，
// 下面是横向滚动的账号卡片。代理模式、TUN、DNS 防泄漏在「高级 · 线路」；
// 严格线路逐组明细在「高级 · 出口检测」；Claude 遥测提示在「用量」页。
export default function Home() {
  const update = useAppUpdate()
  const { message, modal } = App.useApp()
  const [status, setStatus] = useState<DesktopSummary>()
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  const [rules, setRules] = useState<RulesRefresh>()
  const [refreshing, setRefreshing] = useState(false)

  const load = useCallback(() => {
    api.desktop
      .status()
      .then((s) => {
        setStatus(s)
        setError(undefined)
      })
      .catch((e: Error) => setError(e.message))
  }, [])

  useEffect(() => {
    load()
    const timer = setInterval(load, 3000)
    return () => clearInterval(timer)
  }, [load])

  const run = async (action: () => Promise<unknown>) => {
    setBusy(true)
    try {
      await action()
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setBusy(false)
      load()
    }
  }

  const refreshRules = async () => {
    setRefreshing(true)
    try {
      const r = await api.network.refreshRules()
      setRules(r)
      if (r.updated) message.success('规则已更新')
      else message.info('规则已是最新')
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setRefreshing(false)
      load()
    }
  }

  const enableProtection = async (force: boolean): Promise<void> => {
    try {
      await api.killswitch.enable(force)
    } catch (e) {
      if (e instanceof ApiRequestError && e.body.code === 'not_ready') {
        modal.confirm({
          title: '现在启用会切断受保护的服务',
          content: `${e.message} 在内核恢复之前，Claude 等受保护的服务都无法访问，包括这台电脑上正在使用它们的程序。`,
          okText: '仍然启用',
          okButtonProps: { danger: true },
          cancelText: '取消',
          onOk: () => run(() => enableProtection(true)),
        })
        return
      }
      throw e
    }
  }

  // 启用前先经每个严格组探测出口，让用户看到结果再决定。内核没运行时没有可探测的线路，直接走原来的检查。
  const confirmEnable = async (): Promise<void> => {
    const strict = await api.desktop.strictCheck()
    if (strict.groups.length === 0) return enableProtection(false)
    const blocked = strict.required && !!strict.reason
    modal.confirm({
      title: blocked ? '严格线路的出口不符合要求' : '严格线路的出口',
      width: 520,
      content: (
        <div>
          <StrictRows strict={strict} />
          <p className="hint setting-note">
            {blocked
              ? `${strict.reason}。现在启用会立即阻断受保护的服务，直到所有严格线路的出口都符合要求。`
              : strict.required
                ? '所有严格线路的出口都在管理服务允许的列表里。启用后持续探测，任何一条不符合就阻断受保护的服务。'
                : '管理服务没有下发允许的出口，只展示探测结果，不按出口阻断。'}
          </p>
        </div>
      ),
      okText: blocked ? '仍然启用' : '启用',
      okButtonProps: blocked ? { danger: true } : undefined,
      cancelText: '取消',
      onOk: () => run(() => enableProtection(blocked)),
    })
  }

  if (!status) {
    return error ? (
      <Alert type="error" showIcon title="连不上后台服务" description={error} />
    ) : (
      <div className="desktop-stack">
        <Card>
          <Skeleton active title={false} paragraph={{ rows: 1 }} />
        </Card>
        <div className="desktop-columns">
          <Card>
            <Skeleton active paragraph={{ rows: 3 }} />
          </Card>
          <Card>
            <Skeleton active paragraph={{ rows: 3 }} />
          </Card>
        </div>
      </div>
    )
  }
  const { core, killswitch: ks, network, server, strict } = status
  // 绑定管理服务后，下发的节点也算在内。
  const nodeTotal = core.node_count + (server?.bound ? server.nodes : 0)
  const state = ksStates[ks.state]
  const coreState = core.running ? '运行中' : core.desired ? '启动中' : '已停止'
  const tun = tunState(core)
  const eg = network.egress
  const sum = strict && strict.groups.length > 0 ? strictSummary(strict) : undefined
  const overall = overallState(status)

  return (
    <div className="desktop-stack">
      {error && <Alert type="error" showIcon title="连不上后台服务" description={error} />}
      {core.last_error && <Alert type="error" showIcon title="配置未生效，沿用上一份" description={core.last_error} />}
      {ks.dns_guard?.error && <Alert type="error" showIcon title={ks.dns_guard.error} />}
      {/* 受保护域名能解析、国内域名不能：国内 DNS 上游都不通，大部分网站会打不开 */}
      {core.running && ks.health.dns_ok && ks.health.domestic_dns_ok === false && (
        <Alert type="error" showIcon title="国内域名解析失败" description={ks.health.domestic_dns_detail || undefined} />
      )}
      {core.running && core.tun_error && <Alert type="error" showIcon title="TUN 未启动" description={core.tun_error} />}
      {core.warnings.map((w) => (
        <Alert key={w} type="warning" showIcon title={w} />
      ))}
      {nodeTotal === 0 && (
        <Alert
          type="info"
          showIcon
          title={
            <>
              还没有节点：<Link to="/advanced/server">绑定管理服务</Link> 或 <Link to="/advanced/subscriptions">导入订阅</Link>
            </>
          }
        />
      )}
      {server?.bound && server.last_error && (
        <Alert type="warning" showIcon title="管理服务同步失败，沿用上次配置" description={server.last_error} />
      )}
      <AppUpdateBanner update={update} />
      {rules && <RulesResult result={rules} onClose={() => setRules(undefined)} />}

      {/* 状态分两条：上面一条是总体状态和两个主开关，下面一行是要点（出口、网络、代理、规则、DNS、严格线路），
          每格只有标签和一个值，细节和设置在「高级」里。账号排在下面横向滚动，整页尽量一屏放下。 */}
      <section id="overview-status" className="home-hero" data-tone={overall.tone} aria-label="状态" aria-live="polite">
        <span className="home-hero-dot" aria-hidden />
        <div className="home-hero-text">
          <div className="home-hero-title">{overall.title}</div>
          {(() => {
            const desc = overall.desc ?? state.desc
            return (
              <div className="hint selectable home-hero-desc" title={desc}>
                {desc}
              </div>
            )
          })()}
        </div>
        <div className="home-hero-switches">
          <label className="home-switch">
            <span>代理</span>
            <Switch
              aria-label="代理开关"
              checked={core.desired}
              loading={busy || core.applying}
              onChange={(on) => run(() => (on ? api.desktop.start() : api.desktop.stop()))}
            />
          </label>
          <label className="home-switch">
            <span>保护服务</span>
            <Switch
              aria-label="保护服务开关"
              checked={ks.enabled}
              loading={busy}
              onChange={(on) => run(() => (on ? confirmEnable() : api.killswitch.disable()))}
            />
          </label>
        </div>
      </section>

      <section className="home-facts" aria-label="要点">
        <Fact
          label="出口"
          action={
            network.probing && (
              <Tooltip title="重新探测出口">
                <button type="button" className="fact-action" aria-label="重新探测出口" onClick={() => void run(api.network.recheck)}>
                  <ReloadOutlined />
                </button>
              </Tooltip>
            )
          }
          title={eg.error}
        >
          {network.probing && eg.ok ? (
            <>
              <CountryFlag code={eg.country} />
              <span className="selectable">{eg.ip}</span>
            </>
          ) : (
            egressLabel(network)
          )}
        </Fact>
        <Fact label="网络" title={network.box?.addr ? `路由盒 ${network.box.addr}` : undefined}>
          <span className="fact-ellipsis selectable">{networkLabel(network)}</span>
          {network.box?.id ? (
            <Tag color="green">路由盒</Tag>
          ) : (
            network.network.ssid && (network.trusted ? <Tag color="green">受信</Tag> : <Tag>不受信</Tag>)
          )}
        </Fact>
        <Fact label="代理" sub={`${modeLabels[core.mode]}${core.tun ? ' · TUN' : ''} · ${nodeTotal} 节点`} title={`TUN ${tun.label}`}>
          <Badge status={core.running ? 'success' : core.desired ? 'processing' : 'default'} />
          {coreState}
        </Fact>
        <Fact
          label="规则"
          sub={server?.bound ? `${server.rules} 条${server.last_sync ? ` · ${new Date(server.last_sync).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}` : ''}` : '未绑定'}
          action={
            <Tooltip title="刷新规则">
              <button type="button" className="fact-action" aria-label="刷新规则" disabled={refreshing} onClick={() => void refreshRules()}>
                <SyncOutlined spin={refreshing} />
              </button>
            </Tooltip>
          }
        >
          {server?.bound ? `第 ${server.version} 版` : '本机规则'}
        </Fact>
        <Fact label="DNS 防泄漏" to="/advanced/protection">
          {ks.dns_guard?.active ? (
            <Badge status="success" text={ks.dns_guard.delegated ? '交给路由盒' : '生效中'} />
          ) : ks.dns_guard?.enabled ? (
            <Badge status="warning" text="未生效" />
          ) : (
            <Badge status="default" text="未开启" />
          )}
        </Fact>
        <Fact label="严格线路" to="/advanced/egress">
          {sum ? (
            <Badge status={sum.color === 'green' ? 'success' : sum.color === 'red' ? 'error' : 'default'} text={sum.label} />
          ) : (
            <span className="hint">没有</span>
          )}
        </Fact>
      </section>

      {((network.probing && network.baseline && eg.ok && eg.ip !== network.baseline) || (network.network.ssid && !network.trusted)) && (
        <div className="home-net-actions">
          {network.probing && network.baseline && eg.ok && eg.ip !== network.baseline && (
            <>
              <span className="hint">出口已变（原 {network.baseline}）</span>
              <Button size="small" type="primary" icon={<CheckOutlined />} onClick={() => run(api.network.acceptEgress)}>
                接受新出口
              </Button>
            </>
          )}
          {network.network.ssid && !network.trusted && (
            <Tooltip title="在这个 Wi-Fi 上交给路由盒">
              <Button size="small" onClick={() => run(api.network.trustCurrent)}>
                信任当前 Wi-Fi
              </Button>
            </Tooltip>
          )}
        </div>
      )}

      <Accounts embedded />
      <AppVersionLine update={update} />
    </div>
  )
}

// 首页要点行的一格：小标签在上，值在下（可带一行说明）；右上角可放一个小操作，或整格链接到设置页。
function Fact({ label, sub, action, to, title, children }: { label: string; sub?: string; action?: ReactNode; to?: string; title?: string; children: ReactNode }) {
  const body = (
    <>
      <div className="fact-label">
        {label}
        {to && <RightOutlined className="fact-go" />}
      </div>
      <div className="fact-value">{children}</div>
      {sub && <div className="fact-sub hint">{sub}</div>}
    </>
  )
  return (
    <div className="home-fact" title={title}>
      {to ? (
        <Link to={to} className="fact-link">
          {body}
        </Link>
      ) : (
        body
      )}
      {action && <span className="fact-actions">{action}</span>}
    </div>
  )
}

// 总体状态（首页、精简界面共用）：一句标题 + 一句原因。路由盒、受信网络、保护服务的异常优先，其次是代理本身。
export function overallState(s: DesktopSummary): { title: string; desc?: string; tone: Tone } {
  const { core, killswitch: ks, network } = s
  const box = network.box?.id
  if (ks.verdict.path === 'direct' && box) return { title: `经路由盒「${box}」`, desc: ks.verdict.reason, tone: 'ok' }
  if (ks.verdict.path === 'direct') return { title: '受信网络，直连', desc: ks.verdict.reason, tone: 'ok' }
  if (ks.enabled && ks.state !== 'protecting' && ks.state !== 'off') {
    const st = ksStates[ks.state]
    return { title: st.label, desc: ks.verdict.reason || st.desc, tone: st.tone }
  }
  if (!core.desired) return { title: '代理已关闭', desc: '打开开关开始使用', tone: 'off' }
  if (core.last_error) return { title: '代理运行异常', desc: core.last_error, tone: 'bad' }
  if (!core.running) return { title: '代理启动中', tone: 'warn' }
  return { title: ks.enabled ? '代理运行中 · 已保护' : '代理运行中', desc: ks.verdict.reason || undefined, tone: 'ok' }
}

// 刷新规则的结果：有更新时醒目提示（绿色、列出变化），没有更新时一行说明；可关闭。
function RulesResult({ result: r, onClose }: { result: RulesRefresh; onClose: () => void }) {
  const changedSets = r.rule_sets.filter((x) => !x.error && x.after !== x.before)
  const failed = r.rule_sets.filter((x) => x.error)
  const lines: string[] = []
  if (r.bound && r.after_version !== r.before_version) lines.push(`管理服务下发的规则：第 ${r.before_version} 版 → 第 ${r.after_version} 版，已重新生成配置`)
  for (const x of changedSets) lines.push(`规则集「${x.name}」：${x.before} → ${x.after} 条`)
  if (r.profile_error) lines.push(`从管理服务拉取失败：${r.profile_error}`)
  for (const x of failed) lines.push(`规则集「${x.name}」下载失败：${x.error}`)
  if (!r.core_running) lines.push('代理没有在运行，远程规则集要等启动后由内核下载')
  if (r.updated) {
    return (
      <Alert
        className="rules-updated"
        type="success"
        showIcon
        icon={<CheckCircleFilled />}
        title="规则已更新"
        description={
          <ul className="simulate-list">
            {lines.map((l) => (
              <li key={l}>{l}</li>
            ))}
          </ul>
        }
        closable={{ onClose }}
      />
    )
  }
  return (
    <Alert
      type={r.profile_error || failed.length ? 'warning' : 'info'}
      showIcon
      title={`规则已是最新${r.rule_sets.length ? `（检查了 ${r.rule_sets.length} 个远程规则集）` : ''}`}
      description={lines.length ? lines.join('；') : undefined}
      closable={{ onClose }}
    />
  )
}
