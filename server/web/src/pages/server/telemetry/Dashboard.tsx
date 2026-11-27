import { useEffect, useMemo, useState } from 'react'
import { Alert, Card, Empty, Flex, Segmented, Skeleton, Table } from 'antd'
import { ApartmentOutlined, BarChartOutlined, LineChartOutlined, RobotOutlined } from '@ant-design/icons'
import {
  serverApi,
  type ClaudeOverview,
  type Device,
  type RankItem,
  type TelemetryFilter,
  type TelemetryOverview,
  type TelemetryRangeName,
  type Topology,
} from '../../../serverApi'
import KpiBar from '../KpiBar'
import { formatBytes } from '../../common'
import { compact, usd } from '../../claudeUsage'
import EChart, { type ChartClick } from './EChart'
import { bytesOf, rankOption, sankeyOption, trendOption } from './charts'

const claudeDays: Record<TelemetryRangeName, number> = { '1h': 1, '24h': 1, '7d': 7, '30d': 30 }

// 遥测总览（BXC-F0-051）：关键指标、流量趋势、拓扑图、排行、Claude Code 用量；点任何一项进细粒度查看并带上筛选。
export default function Dashboard({ range, onDrill }: { range: TelemetryRangeName; onDrill: (f: TelemetryFilter) => void }) {
  const [data, setData] = useState<TelemetryOverview>()
  const [claude, setClaude] = useState<ClaudeOverview>()
  const [topo, setTopo] = useState<Topology>()
  const [metric, setMetric] = useState<'bytes' | 'conns'>('bytes')
  const [devices, setDevices] = useState<Device[]>([])
  const [error, setError] = useState<string>()

  useEffect(() => {
    serverApi.devices().then(setDevices, () => undefined)
  }, [])

  useEffect(() => {
    let live = true
    const load = () =>
      Promise.all([serverApi.telemetryOverview(range), serverApi.telemetryTopology(range), serverApi.telemetryClaude(claudeDays[range])]).then(
        ([o, t, c]) => {
          if (!live) return
          setData(o)
          setTopo(t)
          setClaude(c)
          setError(undefined)
        },
        (e: Error) => live && setError(e.message),
      )
    void load()
    const timer = setInterval(load, 60000)
    return () => {
      live = false
      clearInterval(timer)
    }
  }, [range])

  const trend = useMemo(() => (data ? trendOption(data.trend, data.range.step) : undefined), [data])
  const sankey = useMemo(() => (topo ? sankeyOption(topo, metric) : undefined), [topo, metric])

  if (error) return <Alert type="error" showIcon title={error} />
  if (!data) return <Skeleton active paragraph={{ rows: 8 }} />

  const t = claude?.summary.totals
  const tokens = t ? t.tokens.input + t.tokens.output + t.tokens.cache_read + t.tokens.cache_creation : 0
  const days = claudeDays[range]

  return (
    <Flex vertical gap={16}>
      <KpiBar
        items={[
          { label: '在线客户端', value: `${data.online} / ${data.clients}`, title: '最近 5 分钟联系过管理服务的 / 全部未吊销的' },
          { label: '有代理流量', value: data.active, title: '这段时间有代理流量的客户端（路由盒下的设备分开算）' },
          {
            label: '代理流量',
            value: formatBytes(bytesOf(data.total)),
            title: `上传 ${formatBytes(data.total.upload)}，下载 ${formatBytes(data.total.download)}；不含直连（DIRECT）`,
          },
          { label: '未归属', value: formatBytes(data.unattributed), title: '内核总流量中归不到具体连接的部分（极短的连接，可能是代理也可能是直连）' },
          { label: '新连接', value: compact(data.total.conns) },
          { label: `Claude 费用（${days === 1 ? '今天' : `${days} 天`}）`, value: t ? usd(t.cost_usd) : '—', title: '按 API 价格估算' },
          { label: 'Claude Tokens', value: t ? compact(tokens) : '—' },
          { label: 'Claude 会话', value: t ? t.sessions : '—' },
        ]}
      />

      <Card
        title={
          <span className="card-title">
            <LineChartOutlined />
            代理流量趋势
          </span>
        }
      >
        {trend && <EChart option={trend} height={240} />}
      </Card>

      <Card
        title={
          <span className="card-title">
            <ApartmentOutlined />
            流量拓扑
          </span>
        }
        extra={
          <Segmented<'bytes' | 'conns'>
            size="small"
            value={metric}
            onChange={setMetric}
            options={[
              { label: '流量', value: 'bytes' },
              { label: '连接数', value: 'conns' },
            ]}
          />
        }
      >
        {topo && topo.links.length > 0 && sankey ? (
          <EChart
            option={sankey}
            height={Math.max(320, Math.min(720, topo.nodes.filter((n) => n.kind === 'host').length * 28))}
            onClick={(p: ChartClick) => {
              const d = p.data as { raw?: { filter?: TelemetryFilter; source?: string; target?: string } }
              if (p.dataType === 'node' && d.raw?.filter && Object.keys(d.raw.filter).length) onDrill(d.raw.filter)
              if (p.dataType === 'edge' && d.raw?.source && d.raw.target) {
                const a = topo.nodes.find((n) => n.id === d.raw?.source)?.filter ?? {}
                const b = topo.nodes.find((n) => n.id === d.raw?.target)?.filter ?? {}
                onDrill({ ...a, ...b })
              }
            }}
          />
        ) : (
          <Empty description="这段时间没有代理流量" />
        )}
        <p className="hint">客户端 → 链路各跳节点 → 目标地址；每层只显示前 12 个，其余合并为「(其他)」。点节点或连线查看明细。</p>
      </Card>

      <div className="telemetry-grid">
        <RankCard title="客户端" items={data.top_clients} onDrill={(k) => onDrill({ client: k })} />
        <RankCard title="目标地址" items={data.top_hosts} onDrill={(k) => onDrill({ host: k })} />
        <RankCard title="出站节点" items={data.top_nodes} onDrill={(k) => onDrill({ node: k })} />
        <RankCard title="规则" items={data.top_rules} onDrill={(k) => onDrill({ rule: k })} />
      </div>

      <Card
        title={
          <span className="card-title">
            <RobotOutlined />
            Claude Code 用量（{days === 1 ? '今天' : `最近 ${days} 天`}）
          </span>
        }
      >
        {claude && (claude.summary.totals.requests || claude.devices.length) ? (
          <div className="telemetry-grid">
            <Table
              size="small"
              rowKey="model"
              pagination={false}
              dataSource={claude.summary.models}
              columns={[
                { title: '模型', dataIndex: 'model' },
                { title: '请求', dataIndex: 'requests', width: 80 },
                { title: 'Tokens', dataIndex: 'tokens', width: 90, render: compact },
                { title: '费用', dataIndex: 'cost_usd', width: 90, render: usd },
              ]}
            />
            <Table
              size="small"
              rowKey="device_id"
              pagination={false}
              dataSource={claude.devices}
              onRow={(d) => ({ onClick: () => onDrill({ client: `d${d.device_id}` }), className: 'clickable-row' })}
              columns={[
                { title: '客户端', dataIndex: 'device_id', render: (id: number) => devices.find((d) => d.id === id)?.name ?? `#${id}` },
                { title: '会话', dataIndex: 'sessions', width: 70 },
                { title: 'Tokens', dataIndex: 'tokens', width: 90, render: compact },
                { title: '费用', dataIndex: 'cost_usd', width: 90, render: usd },
              ]}
            />
          </div>
        ) : (
          <Empty description="没有 Claude Code 用量数据" />
        )}
      </Card>
    </Flex>
  )
}

function RankCard({ title, items, onDrill }: { title: string; items: RankItem[]; onDrill: (key: string) => void }) {
  const option = useMemo(() => rankOption(items), [items])
  return (
    <Card
      size="small"
      title={
        <span className="card-title">
          <BarChartOutlined />
          {title}
        </span>
      }
    >
      {items.length ? (
        <EChart
          option={option}
          height={Math.max(120, items.length * 26)}
          onClick={(p) => {
            const it = [...items].reverse()[p.dataIndex ?? -1]
            if (it) onDrill(it.key)
          }}
        />
      ) : (
        <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="没有数据" />
      )}
    </Card>
  )
}
