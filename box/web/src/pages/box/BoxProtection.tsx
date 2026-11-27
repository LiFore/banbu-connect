import { useCallback, useEffect, useState } from 'react'
import { Alert, Card, Skeleton } from 'antd'
import { HistoryOutlined } from '@ant-design/icons'
import { api, type BoxSummary } from '../../api'
import KpiBar from '../desktop/KpiBar'
import ProtectedServicesCard, { EventTimeline } from '../desktop/ProtectedServices'
import { boxProtection } from './BoxHome'
import HealthCard from './HealthCard'
import ExpansionCard from './ExpansionCard'

const time = (t?: string) => (t ? new Date(t).toLocaleTimeString() : '—')

// 路由盒的 Kill Switch 页：受保护的服务与桌面相同，状态和事件来自 guard。
export default function BoxProtection() {
  const [status, setStatus] = useState<BoxSummary>()

  const load = useCallback(() => {
    api.box.status().then(setStatus, () => undefined)
  }, [])

  useEffect(() => {
    load()
    const timer = setInterval(load, 3000)
    return () => clearInterval(timer)
  }, [load])

  const guard = status?.guard
  const hero = status && boxProtection(status)

  return (
    <div className="desktop-stack">
      {status && hero ? (
        <KpiBar
          items={[
            { label: '状态', value: hero.label, title: hero.desc },
            { label: '防火墙规则', value: guard ? (guard.installed ? '已安装' : '未安装') : '不可达', title: guard?.last_error || status.guard_error },
            { label: '域名 / 网段', value: guard ? `${guard.domains ?? 0} / ${guard.cidrs ?? 0}` : '—' },
            { label: '局域网网桥', value: guard?.lan_device || '—' },
            { label: '最近自检', value: time(guard?.last_check) },
            { label: '策略版本', value: guard?.policy_version || '—' },
          ]}
        />
      ) : (
        <Card>
          <Skeleton active title={false} paragraph={{ rows: 1 }} />
        </Card>
      )}
      {status && !guard && <Alert type="error" showIcon title="guard 没有运行" description={status.guard_error} />}
      {guard?.last_error && <Alert type="error" showIcon title={guard.last_error} />}

      <div className="protection-layout">
        <div className="desktop-stack">
          <ProtectedServicesCard onSaved={load} />
          <HealthCard value={status?.killswitch.health} onSaved={load} />
          <ExpansionCard value={status?.strict_expansion} />
          <Card>
            <p className="hint">
              受保护服务的固定网段和 DNS 解析出的地址进入防火墙集合：局域网设备和路由盒自身都不能直连这些地址，只能经内核的严格代理组出去。
              内核、面板停止或崩溃时规则仍然有效；防火墙重载后 5 秒内自动恢复。路由盒不转发 IPv6，局域网里手工指定的 DNS 也会被交给路由盒解析。
            </p>
          </Card>
        </div>

        <Card
          title={
            <span className="card-title">
              <HistoryOutlined />
              最近事件
            </span>
          }
          className="events-card"
        >
          {!status ? <Skeleton active title={false} paragraph={{ rows: 4 }} /> : <EventTimeline events={guard?.events} empty="暂无事件（guard 重启后清空）。" />}
        </Card>
      </div>
    </div>
  )
}
