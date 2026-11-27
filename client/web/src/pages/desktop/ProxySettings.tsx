import { useCallback, useEffect, useState } from 'react'
import { App, Card, Segmented, Skeleton, Switch, Tag } from 'antd'
import { ApiOutlined } from '@ant-design/icons'
import { api, type CoreStatus, type DesktopSummary, type KillSwitchStatus, type ProxyMode } from '../../api'

// DNS 防泄漏只在 TUN 接管时生效：拦截物理网卡上的 53、853 端口。
function guardTag(g: KillSwitchStatus['dns_guard'] | undefined, core: CoreStatus): { label: string; color: string } {
  if (!g?.enabled) return { label: '已关闭', color: 'default' }
  if (g.error) return { label: '失败', color: 'red' }
  if (g.delegated) return { label: '交给路由盒', color: 'green' }
  if (g.active) return { label: '已生效', color: 'green' }
  if (!core.tun_active) return { label: '等待 TUN', color: 'default' }
  return { label: '启动中', color: 'orange' }
}

// TUN 的实际状态：开关只表示设置，真正是否接管要看内核。
export function tunState(core: CoreStatus): { label: string; color: string } {
  if (!core.tun) return { label: '已关闭', color: 'default' }
  if (!core.running) return { label: '未运行', color: 'default' }
  if (core.tun_active) return { label: '已接管', color: 'green' }
  if (core.tun_error) return { label: '启动失败', color: 'red' }
  return { label: '启动中', color: 'orange' }
}

// 高级 · 线路顶部的代理设置（BXC-F0-048 从概览移来）：代理模式、接管全部流量（TUN）、DNS 防泄漏。
export default function ProxySettings() {
  const { message } = App.useApp()
  const [status, setStatus] = useState<DesktopSummary>()
  const [busy, setBusy] = useState(false)

  const load = useCallback(() => {
    api.desktop.status().then(setStatus, () => undefined)
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

  if (!status) {
    return (
      <Card>
        <Skeleton active title={false} paragraph={{ rows: 2 }} />
      </Card>
    )
  }
  const { core, killswitch: ks } = status
  const guard = guardTag(ks.dns_guard, core)
  const tun = tunState(core)
  return (
    <Card
      title={
        <span className="card-title">
          <ApiOutlined />
          代理设置
        </span>
      }
    >
      <div className="setting-row">
        <span>模式</span>
        <Segmented<ProxyMode>
          value={core.mode}
          disabled={busy}
          options={[
            { label: '规则', value: 'rule' },
            { label: '全局', value: 'global' },
            { label: '直连', value: 'direct' },
          ]}
          onChange={(mode) => run(() => api.desktop.updateSettings({ mode }))}
        />
      </div>
      <div className="setting-row">
        <span>接管全部流量（TUN）</span>
        <span className="setting-control">
          <Tag color={tun.color}>{tun.label}</Tag>
          <Switch aria-label="接管全部流量" checked={core.tun} loading={busy} onChange={(on) => run(() => api.desktop.updateSettings({ tun: on }))} />
        </span>
      </div>
      <div className="setting-row">
        <span>DNS 防泄漏</span>
        <span className="setting-control">
          <Tag color={guard.color} title={ks.dns_guard?.error}>
            {guard.label}
          </Tag>
          <Switch
            aria-label="DNS 防泄漏"
            checked={ks.dns_guard?.enabled ?? true}
            loading={busy}
            onChange={(on) => run(() => api.desktop.updateSettings({ dns_guard: on }))}
          />
        </span>
      </div>
    </Card>
  )
}
