import { useEffect, useMemo, useState } from 'react'
import { Alert, Button, Card, Drawer, Empty, Flex, Input, Select, Skeleton, Space, Table, Tabs, Tag, Tooltip } from 'antd'
import { FilterOutlined, LineChartOutlined, RobotOutlined, ThunderboltOutlined, UnorderedListOutlined } from '@ant-design/icons'
import {
  serverApi,
  type Device,
  type LiveView,
  type RankItem,
  type TelemetryDetail,
  type TelemetryFilter,
  type TelemetryRangeName,
  type TelemetryRecord,
} from '../../../serverApi'
import type { OTelSummary } from '../../../api'
import KpiBar from '../KpiBar'
import { formatBytes } from '../../common'
import { compact, DaysPicker, UsageView } from '../../claudeUsage'
import EChart from './EChart'
import { bytesOf, trendOption } from './charts'

const time = (v: string) => new Date(v).toLocaleString([], { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' })

const filterLabels: Record<keyof TelemetryFilter, string> = {
  client: '客户端',
  host: '目标地址',
  rule: '规则',
  node: '出站节点',
  group: '代理组',
  hop: '经过节点',
}

// 细粒度查看（BXC-F0-028）：按客户端（含路由盒下的设备）、时间、目标地址、规则、代理组、节点筛选；
// 流量曲线、各维度汇总、连接明细；筛的是一台客户端时还有它的 Claude Code 会话和实时连接。
export default function Detail({
  range,
  filter,
  onFilter,
}: {
  range: TelemetryRangeName
  filter: TelemetryFilter
  onFilter: (f: TelemetryFilter) => void
}) {
  const [data, setData] = useState<TelemetryDetail>()
  const [more, setMore] = useState<TelemetryRecord[]>([])
  const [loadingMore, setLoadingMore] = useState(false)
  const [devices, setDevices] = useState<Device[]>([])
  const [error, setError] = useState<string>()
  const [host, setHost] = useState(filter.host ?? '')
  const [live, setLive] = useState(false)
  const key = JSON.stringify(filter)

  useEffect(() => {
    serverApi.devices().then(setDevices, () => undefined)
  }, [])

  useEffect(() => {
    // filter 每次渲染都是新对象，按内容（key）比较；筛选变了时整个组件重新挂载（见 Telemetry.tsx），状态从头开始。
    const f = JSON.parse(key) as TelemetryFilter
    let alive = true
    serverApi.telemetryDetail(range, f).then(
      (d) => {
        if (!alive) return
        setData(d)
        setError(undefined)
      },
      (e: Error) => alive && setError(e.message),
    )
    return () => {
      alive = false
    }
  }, [range, key])

  const trend = useMemo(() => (data ? trendOption(data.trend, data.range.step, !filter.host && !filter.rule && !filter.node) : undefined), [data, filter])
  const records = useMemo(() => [...(data?.records ?? []), ...more], [data, more])
  const add = (f: TelemetryFilter) => onFilter({ ...filter, ...f })

  const loadMore = async () => {
    const last = records[records.length - 1]
    if (!last) return
    setLoadingMore(true)
    try {
      const d = await serverApi.telemetryDetail(range, filter, last.id)
      setMore((m) => [...m, ...d.records])
    } finally {
      setLoadingMore(false)
    }
  }

  // 客户端选项：所有客户端，加上当前筛选的（路由盒下的设备）和数据里出现的。
  const clientOptions = useMemo(() => {
    const m = new Map<string, string>()
    devices.filter((d) => !d.revoked).forEach((d) => m.set(`d${d.id}`, d.name))
    data?.clients.forEach((c) => m.set(c.key, c.label))
    if (filter.client && !m.has(filter.client)) m.set(filter.client, filter.client)
    return [...m].map(([value, label]) => ({ value, label }))
  }, [devices, data, filter.client])

  // 实时连接按客户端本身看（路由盒下的设备看的是整台路由盒）。
  const liveDevice = filter.client ? Number(filter.client.slice(1).split(':')[0]) : 0
  const liveName = devices.find((d) => d.id === liveDevice)?.name ?? `#${liveDevice}`

  return (
    <Flex vertical gap={16}>
      <Card size="small">
        <div className="page-toolbar telemetry-filters">
          <FilterOutlined />
          <Select
            allowClear
            showSearch
            optionFilterProp="label"
            placeholder="全部客户端"
            style={{ width: 240 }}
            value={filter.client}
            options={clientOptions}
            onChange={(v?: string) => onFilter({ ...filter, client: v })}
          />
          <Input.Search
            allowClear
            placeholder="目标地址（域名或 IP）"
            style={{ width: 240 }}
            value={host}
            onChange={(e) => setHost(e.target.value)}
            onSearch={(v) => onFilter({ ...filter, host: v.trim() || undefined })}
          />
          {(Object.keys(filterLabels) as (keyof TelemetryFilter)[])
            .filter((k) => k !== 'client' && k !== 'host' && filter[k])
            .map((k) => (
              <Tag key={k} closable onClose={() => onFilter({ ...filter, [k]: undefined })}>
                {filterLabels[k]}：{filter[k]}
              </Tag>
            ))}
          <span className="spacer" />
          {liveDevice > 0 && (
            <Button icon={<ThunderboltOutlined />} onClick={() => setLive(true)}>
              实时连接
            </Button>
          )}
        </div>
      </Card>

      {error && <Alert type="error" showIcon title={error} />}
      {!data ? (
        <Skeleton active paragraph={{ rows: 8 }} />
      ) : (
        <>
          <KpiBar
            items={[
              { label: '代理流量', value: formatBytes(bytesOf(data.total)) },
              { label: '上传', value: formatBytes(data.total.upload) },
              { label: '下载', value: formatBytes(data.total.download) },
              { label: '新连接', value: compact(data.total.conns) },
            ]}
          />
          <Card
            title={
              <span className="card-title">
                <LineChartOutlined />
                流量曲线
              </span>
            }
          >
            {trend && <EChart option={trend} height={220} />}
          </Card>
          <Card size="small">
            <Tabs
              items={[
                { key: 'host', label: '目标地址', children: <RankTable items={data.hosts} onPick={(k) => add({ host: k })} /> },
                { key: 'rule', label: '规则', children: <RankTable items={data.rules} onPick={(k) => add({ rule: k })} /> },
                { key: 'node', label: '出站节点', children: <RankTable items={data.nodes} onPick={(k) => add({ node: k })} /> },
                { key: 'group', label: '代理组', children: <RankTable items={data.groups} onPick={(k) => add({ group: k })} /> },
                { key: 'client', label: '客户端', children: <RankTable items={data.clients} onPick={(k) => add({ client: k })} /> },
                { key: 'process', label: '程序', children: <RankTable items={data.processes.filter((p) => p.key)} /> },
              ]}
            />
          </Card>
          <Card
            title={
              <span className="card-title">
                <UnorderedListOutlined />
                连接明细
              </span>
            }
            extra={<span className="hint">已关闭的连接，保留 7 天</span>}
          >
            <RecordTable records={records} onPick={add} />
            {records.length > 0 && records.length % 200 === 0 && (
              <div className="table-more">
                <Button loading={loadingMore} onClick={loadMore}>
                  加载更多
                </Button>
              </div>
            )}
          </Card>
          {data.device_id ? <ClaudeCard deviceId={data.device_id} /> : null}
        </>
      )}

      <Drawer title={`实时连接：${liveName}`} open={live} size="large" onClose={() => setLive(false)} destroyOnHidden>
        {live && <LiveConns deviceId={liveDevice} />}
      </Drawer>
    </Flex>
  )
}

function RankTable({ items, onPick }: { items: RankItem[]; onPick?: (key: string) => void }) {
  const total = items.reduce((n, i) => n + bytesOf(i), 0) || 1
  return (
    <Table<RankItem>
      size="small"
      rowKey="key"
      dataSource={items}
      pagination={{ pageSize: 10, hideOnSinglePage: true }}
      onRow={(it) => (onPick ? { onClick: () => onPick(it.key), className: 'clickable-row' } : {})}
      locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="没有数据" /> }}
      columns={[
        { title: '名称', dataIndex: 'label', ellipsis: true, render: (v: string) => v || '—' },
        { title: '流量', key: 'bytes', width: 110, render: (_, i) => formatBytes(bytesOf(i)) },
        { title: '占比', key: 'pct', width: 70, render: (_, i) => `${Math.round((bytesOf(i) / total) * 100)}%` },
        { title: '上传 / 下载', key: 'ud', width: 170, render: (_, i) => `${formatBytes(i.upload)} / ${formatBytes(i.download)}` },
        { title: '连接', dataIndex: 'conns', width: 80 },
      ]}
    />
  )
}

function RecordTable({ records, onPick }: { records: TelemetryRecord[]; onPick: (f: TelemetryFilter) => void }) {
  return (
    <Table<TelemetryRecord>
      size="small"
      rowKey="id"
      dataSource={records}
      pagination={false}
      scroll={{ x: 1080, y: 480 }}
      locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="没有连接记录" /> }}
      columns={[
        { title: '结束', dataIndex: 'end', width: 150, render: time },
        {
          title: '时长',
          key: 'dur',
          width: 80,
          render: (_, r) => {
            const s = Math.max(0, (new Date(r.end).getTime() - new Date(r.start).getTime()) / 1000)
            return s >= 60 ? `${Math.round(s / 60)} 分` : `${Math.round(s)} 秒`
          },
        },
        { title: '客户端', dataIndex: 'client_label', width: 160, ellipsis: true },
        {
          title: '目标地址',
          dataIndex: 'host',
          ellipsis: true,
          render: (v: string, r) => (
            <Tooltip title={`${r.network} ${r.destination}`}>
              <a onClick={() => onPick({ host: v })}>{v}</a>
            </Tooltip>
          ),
        },
        { title: '规则', key: 'rule', width: 180, ellipsis: true, render: (_, r) => (r.payload ? `${r.rule},${r.payload}` : r.rule) },
        { title: '链路', dataIndex: 'path', width: 220, ellipsis: true, render: (p: string[]) => p.join(' → ') },
        { title: '程序', dataIndex: 'process', width: 110, ellipsis: true, render: (v?: string) => v || '—' },
        { title: '流量', key: 'bytes', width: 100, render: (_, r) => formatBytes(r.upload + r.download) },
      ]}
    />
  )
}

// 这台客户端上报的 Claude Code 用量（从「客户端」页移过来，BXC-F0-050）。
function ClaudeCard({ deviceId }: { deviceId: number }) {
  const [days, setDays] = useState(7)
  const [data, setData] = useState<OTelSummary>()
  const [error, setError] = useState<string>()
  useEffect(() => {
    serverApi.deviceOTel(deviceId, days).then(
      (d) => {
        setData(d)
        setError(undefined)
      },
      (e: Error) => setError(e.message),
    )
  }, [deviceId, days])
  return (
    <Card
      title={
        <span className="card-title">
          <RobotOutlined />
          Claude Code 用量与会话
        </span>
      }
      extra={<DaysPicker value={days} onChange={setDays} />}
    >
      {error && <Alert type="error" showIcon title={error} />}
      {data && (data.totals.requests || data.totals.sessions ? <UsageView summary={data} /> : <Empty description="所选范围内没有上报数据" />)}
    </Card>
  )
}

// 实时连接（BXC-F0-030）：打开期间每 3 秒取一次，客户端在这期间推送（开始后可能要等十几秒）。
function LiveConns({ deviceId }: { deviceId: number }) {
  const [view, setView] = useState<LiveView>()
  const [error, setError] = useState<string>()
  useEffect(() => {
    let alive = true
    const load = () =>
      serverApi.deviceLive(deviceId).then(
        (v) => {
          if (!alive) return
          setView(v)
          setError(undefined)
        },
        (e: Error) => alive && setError(e.message),
      )
    void load()
    const timer = setInterval(load, 3000)
    return () => {
      alive = false
      clearInterval(timer)
    }
  }, [deviceId])
  if (error) return <Alert type="error" showIcon title={error} />
  if (!view?.at) return <Alert type="info" showIcon title="正在等客户端推送实时连接（客户端每 15 秒检查一次，需要它在线并打开了代理连接统计）…" />
  return (
    <Space orientation="vertical" size={8} style={{ width: '100%' }}>
      <span className="hint">
        {view.conns.length} 条经代理组的连接，更新于 {new Date(view.at).toLocaleTimeString()}
      </span>
      <Table
        size="small"
        rowKey={(c) => `${c.start}|${c.destination}|${c.source ?? ''}|${c.host}`}
        dataSource={view.conns}
        pagination={false}
        scroll={{ y: 'calc(100vh - 220px)' }}
        columns={[
          { title: '目标地址', dataIndex: 'host', ellipsis: true },
          { title: '链路', dataIndex: 'path', ellipsis: true, render: (p: string[]) => p.join(' → ') },
          { title: '来源', key: 'who', width: 120, render: (_, c) => c.source || c.process || '—' },
          { title: '流量', key: 'bytes', width: 90, render: (_, c) => formatBytes(c.upload + c.download) },
          { title: '开始', dataIndex: 'start', width: 90, render: (v: string) => new Date(v).toLocaleTimeString() },
        ]}
      />
    </Space>
  )
}
