import { useSearchParams } from 'react-router-dom'
import { Segmented, Space } from 'antd'
import type { TelemetryFilter, TelemetryRangeName } from '../../../serverApi'
import { PageHeader } from '../../common'
import Dashboard from './Dashboard'
import Detail from './Detail'
import Usage from './Usage'
import AiOnline from './AiOnline'

// 管理服务「遥测」页（BXC-F0-050）：总览（Dashboard、拓扑图、排行、Claude Code）、细粒度查看、流量用量。
// 视图、时间范围和筛选条件都放在地址里，总览上点任何一项都能带着筛选跳到细粒度查看，也能直接分享链接。
// 「AI 账号在线」是每台客户端用每个 AI 账号的在线时长（按切换时间算），有自己的时间范围和筛选。
// 流量都是经代理组的流量，DIRECT 不统计（ADR-048、Q-021）。

export type View = 'overview' | 'detail' | 'usage' | 'ai'

const filterKeys: (keyof TelemetryFilter)[] = ['client', 'host', 'rule', 'node', 'group', 'hop']

export const rangeOptions: { label: string; value: TelemetryRangeName }[] = [
  { label: '1 小时', value: '1h' },
  { label: '24 小时', value: '24h' },
  { label: '7 天', value: '7d' },
  { label: '30 天', value: '30d' },
]

export default function Telemetry() {
  const [params, setParams] = useSearchParams()
  const view = (params.get('view') as View) || 'overview'
  const range = (params.get('range') as TelemetryRangeName) || '24h'
  const filter: TelemetryFilter = Object.fromEntries(filterKeys.map((k) => [k, params.get(k) ?? undefined]).filter(([, v]) => v))

  // 切换视图时不带筛选，除非明确给了（从总览点进细粒度查看）。
  const go = (next: { view?: View; range?: TelemetryRangeName; filter?: TelemetryFilter }) => {
    const p = new URLSearchParams()
    p.set('view', next.view ?? view)
    p.set('range', next.range ?? range)
    const f = next.filter ?? (next.view && next.view !== view ? {} : filter)
    for (const k of filterKeys) if (f[k]) p.set(k, f[k] as string)
    setParams(p)
  }

  return (
    <Space orientation="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader
        title="遥测"
        actions={
          <>
            <Segmented<View>
              value={view}
              onChange={(v) => go({ view: v })}
              options={[
                { label: '总览', value: 'overview' },
                { label: '细粒度查看', value: 'detail' },
                { label: '流量用量', value: 'usage' },
                { label: 'AI 账号在线', value: 'ai' },
              ]}
            />
            {view !== 'usage' && view !== 'ai' && <Segmented<TelemetryRangeName> value={range} onChange={(r) => go({ range: r })} options={rangeOptions} />}
          </>
        }
      />
      {view === 'overview' && <Dashboard range={range} onDrill={(f) => go({ view: 'detail', filter: f })} />}
      {view === 'detail' && <Detail key={`${range}|${params.toString()}`} range={range} filter={filter} onFilter={(f) => go({ filter: f })} />}
      {view === 'usage' && <Usage onDrill={(f) => go({ view: 'detail', filter: f })} />}
      {view === 'ai' && <AiOnline />}
    </Space>
  )
}
