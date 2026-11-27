import { useCallback, useEffect, useState } from 'react'
import { Alert, App, Button, Card, Progress, Segmented, Skeleton, Switch, Tag } from 'antd'
import { Link } from 'react-router-dom'
import { ApiOutlined, GlobalOutlined, ReloadOutlined, SafetyOutlined } from '@ant-design/icons'
import { api, type BoxSummary, type CoreStatus, type GuardStatus, type ProxyMode, type SubscriptionUsage } from '../../api'
import { formatBytes } from '../common'
import KpiBar from '../desktop/KpiBar'
import { StrictRows, type Tone } from '../desktop/Home'

const modeLabels: Record<ProxyMode, string> = { rule: '规则', global: '全局', direct: '直连' }

interface HeroState {
  label: string
  tone: Tone
  desc: string
}

// 路由盒的 Kill Switch：guard 的 nft 规则常驻，受保护的服务只能经内核出去（ADR-032）。
export function boxProtection(s: Pick<BoxSummary, 'core' | 'guard' | 'guard_error' | 'killswitch'>): HeroState {
  const { core, guard, killswitch: ks } = s
  if (!guard) return { label: '保护服务没有运行', tone: 'bad', desc: s.guard_error || 'guard 不可达，受保护的服务可能直连。' }
  if (guard.state === 'error') return { label: '保护规则没有装上', tone: 'bad', desc: guard.last_error || '防火墙规则安装失败。' }
  if (!ks.enabled) return { label: '未启用', tone: 'off', desc: '局域网设备访问受保护的服务时，代理故障可能直连。' }
  if (guard.hold) return { label: '已阻断受保护的服务', tone: 'warn', desc: guard.reason || '严格线路的出口还没确认或不符合要求。' }
  if (!core.running) return { label: '已断开受保护的服务', tone: 'warn', desc: '内核没有运行，受保护的服务被防火墙拦截。启动代理后自动恢复。' }
  return { label: '保护中', tone: 'ok', desc: '局域网访问受保护的服务只能经内核；内核故障时由防火墙拦截。' }
}

function tunState(core: CoreStatus): { label: string; color: string } {
  if (!core.running) return { label: '未运行', color: 'default' }
  if (core.tun_active) return { label: '已接管', color: 'green' }
  if (core.tun_error) return { label: '启动失败', color: 'red' }
  return { label: '启动中', color: 'orange' }
}

function guardRules(g: GuardStatus | null): string {
  if (!g) return '不可达'
  if (!g.installed) return '未安装'
  return `${g.domains ?? 0} 个域名、${g.cidrs ?? 0} 个网段`
}

export default function BoxHome() {
  const { message, modal } = App.useApp()
  const [status, setStatus] = useState<BoxSummary>()
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)

  const load = useCallback(() => {
    api.box
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

  const confirmDisable = () =>
    modal.confirm({
      title: '关闭保护服务？',
      content: '关闭后代理故障时，局域网设备访问 Claude 等受保护的服务会直连。',
      okText: '关闭',
      okButtonProps: { danger: true },
      cancelText: '取消',
      onOk: () => run(api.killswitch.disable),
    })

  if (!status) {
    return error ? (
      <Alert type="error" showIcon title="连不上路由盒面板" description={error} />
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

  const { core, guard, killswitch: ks, server, strict } = status
  const nodeTotal = core.node_count + (server?.bound ? server.nodes : 0)
  const hero = boxProtection(status)
  const coreState = core.running ? '运行中' : core.desired ? '启动中' : '已停止'
  const tun = tunState(core)

  return (
    <div className="desktop-stack">
      {error && <Alert type="error" showIcon title="连不上路由盒面板" description={error} />}
      {core.last_error && <Alert type="error" showIcon title="配置没有生效，内核继续使用上一份配置" description={core.last_error} />}
      {core.running && core.tun_error && (
        <Alert type="error" showIcon title="TUN 没有启动，局域网流量没有被接管" description={core.tun_error} />
      )}
      {guard?.fake && <Alert type="info" showIcon title="开发模式：guard 没有改动防火墙" />}
      {core.warnings.map((w) => (
        <Alert key={w} type="warning" showIcon title={w} />
      ))}
      {nodeTotal === 0 && (
        <Alert
          type="info"
          showIcon
          title="还没有节点"
          description={
            <>
              到 <Link to="/advanced/server">管理服务</Link> 绑定后自动获取，或在 <Link to="/advanced/subscriptions">订阅</Link> 导入、在{' '}
              <Link to="/advanced/nodes">节点</Link> 手动添加。
            </>
          }
        />
      )}
      {server?.bound && server.last_error && (
        <Alert type="warning" showIcon title="管理服务同步失败，继续使用上一次的配置" description={server.last_error} />
      )}
      {core.confirm === 'unconfirmed' && core.confirm_reason && (
        <Alert type="warning" showIcon title="新配置应用后出口不通" description={core.confirm_reason} />
      )}
      {(status.subscription_errors ?? []).map((e) => (
        <Alert
          key={e.name}
          type="warning"
          showIcon
          title={`订阅「${e.name}」更新失败，继续使用上一次的节点`}
          description={
            <>
              {e.error}（<Link to="/advanced/subscriptions">订阅</Link>）
            </>
          }
        />
      ))}

      <section className="ks-hero" data-tone={hero.tone} aria-live="polite">
        <span className="ks-hero-icon">
          <SafetyOutlined />
        </span>
        <div className="ks-hero-text">
          <div className="ks-hero-label">保护服务</div>
          <div className="ks-hero-state">{hero.label}</div>
          <div className="ks-hero-reason selectable">{hero.desc}</div>
        </div>
        <div className="ks-hero-side">
          <Switch
            aria-label="保护服务开关"
            checked={ks.enabled}
            loading={busy}
            onChange={(on) => (on ? run(() => api.killswitch.enable(false)) : confirmDisable())}
          />
        </div>
      </section>

      <KpiBar
        items={[
          { label: '内核', value: coreState },
          { label: '模式', value: modeLabels[core.mode] },
          { label: '节点', value: nodeTotal, title: server?.bound ? `下发 ${server.nodes} 个，本地 ${core.node_count} 个` : undefined },
          { label: '透明代理', value: tun.label },
          { label: '保护规则', value: guardRules(guard) },
          {
            label: '配置版本',
            value: status.config_version ? <Link to="/advanced/versions">#{status.config_version}</Link> : '—',
            title: core.confirm ? confirmLabels[core.confirm] : undefined,
          },
          { label: '程序版本', value: status.version },
        ]}
      />
      {(status.subscriptions ?? []).length > 0 && <SubscriptionUsageCard list={status.subscriptions ?? []} />}

      <div className="desktop-columns">
        <Card
          title={
            <span className="card-title">
              <ApiOutlined />
              代理
            </span>
          }
          extra={
            <Switch
              aria-label="代理开关"
              checked={core.desired}
              loading={busy || core.applying}
              onChange={(on) => run(() => (on ? api.desktop.start() : api.desktop.stop()))}
            />
          }
        >
          <div className="setting-row">
            <span>模式</span>
            <Segmented<ProxyMode>
              value={core.mode}
              options={[
                { label: '规则', value: 'rule' },
                { label: '全局', value: 'global' },
                { label: '直连', value: 'direct' },
              ]}
              onChange={(mode) => run(() => api.desktop.updateSettings({ mode }))}
            />
          </div>
          <div className="setting-row">
            <span>局域网透明代理（TUN）</span>
            <span className="setting-control">
              <Tag color={tun.color}>{tun.label}</Tag>
            </span>
          </div>
          <div className="setting-row">
            <span>局域网 DNS</span>
            <span className="setting-control">
              <Tag color={core.running ? 'green' : 'default'}>{core.running ? '经内核解析' : '运营商 DNS'}</Tag>
            </span>
          </div>
          <p className="hint setting-note">
            局域网设备不用任何设置：流量由路由盒接管，按规则分流。未知流量默认直连，只有受保护的服务在代理故障时会断开。
          </p>
        </Card>

        <Card
          title={
            <span className="card-title">
              <GlobalOutlined />
              严格线路出口
            </span>
          }
          extra={
            strict.groups.length > 0 && (
              <Button size="small" icon={<ReloadOutlined />} aria-label="重新探测严格线路出口" onClick={() => run(api.desktop.strictCheck)} />
            )
          }
        >
          {strict.groups.length > 0 ? (
            <>
              <StrictRows strict={strict} />
              <p className="hint setting-note">
                {strict.required
                  ? `管理服务允许的出口：${strict.allowed.join('、')}。任何一条严格线路不符合，就阻断受保护的服务。`
                  : '管理服务没有下发允许的出口，只展示探测结果。'}
              </p>
            </>
          ) : (
            <p className="hint">内核运行后经每个严格代理组探测出口。</p>
          )}
        </Card>
      </div>
    </div>
  )
}

const confirmLabels: Record<string, string> = {
  pending: '应用后确认中',
  confirmed: '已确认出口可用',
  unconfirmed: '出口不通（判断为上游故障）',
  rolled_back: '已回滚到上一版',
  failed: '确认失败',
}

// 各订阅的用量和到期（M11-01）。
function SubscriptionUsageCard({ list }: { list: SubscriptionUsage[] }) {
  // 剩余天数按页面打开时算，不需要逐秒更新。
  const [now] = useState(() => Date.now())
  return (
    <Card size="small" title={<span className="card-title">订阅</span>}>
      <div className="sub-usage-list">
        {list.map((s) => {
          const pct = s.total > 0 ? Math.min(100, Math.round((s.used / s.total) * 100)) : undefined
          const days = s.expire ? Math.ceil((new Date(s.expire).getTime() - now) / 86400000) : undefined
          return (
            <div key={s.name} className="sub-usage">
              <div className="sub-usage-head">
                <span>{s.name}</span>
                <span className="hint">{s.nodes} 个节点</span>
              </div>
              {pct !== undefined ? (
                <Progress percent={pct} size="small" status={pct >= 90 ? 'exception' : 'normal'} format={() => `${formatBytes(s.used)} / ${formatBytes(s.total)}`} />
              ) : (
                <span className="hint">用量未知</span>
              )}
              <span className={days !== undefined && days <= 7 ? 'hint warn' : 'hint'}>
                {s.expire ? `到期 ${new Date(s.expire).toLocaleDateString()}${days !== undefined ? `（${days > 0 ? `剩 ${days} 天` : '已过期'}）` : ''}` : '到期未知'}
              </span>
            </div>
          )
        })}
      </div>
    </Card>
  )
}
