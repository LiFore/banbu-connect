import { useEffect, useState } from 'react'
import { Alert, Card, Skeleton } from 'antd'
import { HistoryOutlined } from '@ant-design/icons'
import { api, type KillSwitchStatus } from '../../api'
import { ksStates } from './Home'
import KpiBar from './KpiBar'
import TrustedNetwork from './TrustedNetwork'
import ProtectedServicesCard, { EventTimeline } from './ProtectedServices'
import { TrustBypassCard } from './TrustBypass'

export default function Protection() {
  const [status, setStatus] = useState<KillSwitchStatus>()

  useEffect(() => {
    const load = () => api.desktop.status().then((s) => setStatus(s.killswitch), () => undefined)
    void load()
    const timer = setInterval(load, 3000)
    return () => clearInterval(timer)
  }, [])

  const state = status && ksStates[status.state]

  return (
    <div className="desktop-stack">
      {status && state ? (
        <KpiBar
          items={[
            { label: '状态', value: state.label, title: status.verdict?.reason || state.desc },
            { label: '内核 DNS', value: status.health.dns_ok ? '正常' : '异常', title: status.health.dns_detail },
            { label: '流量出口', value: status.health.interface || '未知', title: status.health.interface },
            { label: 'TUN', value: status.health.tun_ok ? '已接管' : '未接管' },
            {
              label: '后缀 / 网段',
              value: `${status.desired.suffixes?.length ?? 0} / ${status.desired.cidrs?.length ?? 0}`,
            },
            { label: '拦截网卡', value: status.desired.interfaces?.join('、') || '无', title: status.desired.interfaces?.join('、') },
          ]}
        />
      ) : (
        <Card>
          <Skeleton active title={false} paragraph={{ rows: 1 }} />
        </Card>
      )}
      {status && !status.health.dns_ok && status.verdict?.path === 'proxy' && <Alert type="warning" showIcon title={`内核 DNS：${status.health.dns_detail}`} />}
      {status?.last_error && <Alert type="error" showIcon title={status.last_error} />}
      {status?.rules.warnings?.map((w) => <Alert key={w} type="warning" showIcon title={w} />)}
      {status?.rule_checks?.filter((c) => c.status !== 'ok' && c.status !== 'unknown').map((c) => (
        <Alert key={c.suffix} type="error" showIcon title={`${c.suffix}：${c.detail}`} />
      ))}

      <div className="protection-layout">
        <div className="desktop-stack">
          <TrustedNetwork />
          <ProtectedServicesCard />
          <TrustBypassCard />
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
          {!status ? <Skeleton active title={false} paragraph={{ rows: 4 }} /> : <EventTimeline events={status.events} />}
        </Card>
      </div>
    </div>
  )
}
