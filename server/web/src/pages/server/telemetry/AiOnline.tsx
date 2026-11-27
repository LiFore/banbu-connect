import { useEffect, useMemo, useState } from 'react'
import { Alert, Card, DatePicker, Empty, Segmented, Select, Skeleton, Space, Table, Tag } from 'antd'
import { FieldTimeOutlined, HistoryOutlined } from '@ant-design/icons'
import type { EChartsCoreOption } from 'echarts/core'
import dayjs, { type Dayjs } from 'dayjs'
import { serverApi, type AIOnlineSession, type AIOnlineStats, type AIOnlineTotal, type OnlineKind } from '../../../serverApi'
import { formatClock, formatDuration } from '../aiOnline'
import EChart from './EChart'

// 遥测「AI 账号在线」：每台客户端用每个账号的在线时长，按切换时间算——
// Gemini 是客户端的当前账号，Claude / Codex 是有进程在运行的工作区（Windows 按设为默认的账号）；
// 切走或客户端离线（3 分钟没有上报）时一段结束。时段由客户端每分钟上报，管理服务记录（dist/ai_online.go）。

type Span = 'today' | '7d' | '30d' | 'custom'

const kindTag: Record<OnlineKind, { label: string; color: string }> = {
  claude: { label: 'Claude', color: 'orange' },
  codex: { label: 'Codex', color: 'blue' },
  gemini: { label: 'Gemini', color: 'green' },
}

const spanOptions: { label: string; value: Span }[] = [
  { label: '今天', value: 'today' },
  { label: '7 天', value: '7d' },
  { label: '30 天', value: '30d' },
  { label: '自定义', value: 'custom' },
]

// 范围按浏览器的本地日期：今天是零点到现在，7 天 / 30 天含今天。
function rangeOf(span: Span, custom: [Dayjs, Dayjs] | null): [Dayjs, Dayjs] {
  const today = dayjs().startOf('day')
  switch (span) {
    case 'today':
      return [today, dayjs()]
    case '30d':
      return [today.subtract(29, 'day'), dayjs()]
    case 'custom':
      if (custom) {
        const end = custom[1].endOf('day')
        return [custom[0].startOf('day'), end.isAfter(dayjs()) ? dayjs() : end]
      }
      return [today.subtract(6, 'day'), dayjs()]
    default:
      return [today.subtract(6, 'day'), dayjs()]
  }
}

const iso = (d: Dayjs) => d.toDate().toISOString().replace(/\.\d{3}Z$/, 'Z')
const accountKey = (kind: string, id: number) => `${kind}:${id}`

function daysOption(stats: AIOnlineStats, from: Dayjs, to: Dayjs, labels: Map<string, string>): EChartsCoreOption {
  const days: string[] = []
  for (let d = from.startOf('day'); !d.isAfter(to); d = d.add(1, 'day')) days.push(d.format('YYYY-MM-DD'))
  // 按总时长取前 8 个账号，其余合成「其他」。
  const sum = new Map<string, number>()
  for (const d of stats.days) sum.set(accountKey(d.kind, d.account_id), (sum.get(accountKey(d.kind, d.account_id)) ?? 0) + d.seconds)
  const order = [...sum.entries()].sort((a, b) => b[1] - a[1]).map(([k]) => k)
  const top = new Set(order.slice(0, 8))
  const series = new Map<string, number[]>()
  for (const d of stats.days) {
    const k = accountKey(d.kind, d.account_id)
    const name = top.has(k) ? (labels.get(k) ?? k) : '其他'
    if (!series.has(name)) series.set(name, days.map(() => 0))
    const i = days.indexOf(d.day)
    if (i >= 0) series.get(name)![i] += d.seconds / 3600
  }
  return {
    grid: { left: 48, right: 16, top: 40, bottom: 28 },
    legend: { top: 0, type: 'scroll' },
    tooltip: { trigger: 'axis', valueFormatter: (v: number) => formatDuration(Math.round(v * 3600)) },
    xAxis: { type: 'category', data: days.map((d) => d.slice(5)) },
    yAxis: { type: 'value', axisLabel: { formatter: (v: number) => `${v} 时` } },
    series: [...series.entries()].map(([name, data]) => ({ name, type: 'bar', stack: 'online', data, barMaxWidth: 24 })),
  }
}

export default function AiOnline() {
  const [span, setSpan] = useState<Span>('7d')
  const [custom, setCustom] = useState<[Dayjs, Dayjs] | null>(null)
  const [device, setDevice] = useState<number>()
  const [kind, setKind] = useState<OnlineKind>()
  const [account, setAccount] = useState<string>() // kind:id
  const [res, setRes] = useState<{ key: string; data?: AIOnlineStats; error?: string }>()

  const [from, to] = useMemo(() => rangeOf(span, custom), [span, custom])
  const acc = account?.split(':')
  const query = {
    from: iso(from),
    to: iso(to),
    device,
    kind: (acc?.[0] as OnlineKind | undefined) ?? kind,
    account: acc ? Number(acc[1]) : undefined,
  }
  const key = JSON.stringify({ span, custom: custom?.map((d) => d.format('YYYY-MM-DD')), device, kind, account })
  const data = res?.key === key ? res.data : undefined
  const error = res?.key === key ? res.error : undefined

  useEffect(() => {
    let alive = true
    serverApi.aiOnline(query).then(
      (d) => alive && setRes({ key, data: d }),
      (e: Error) => alive && setRes({ key, error: e.message }),
    )
    return () => {
      alive = false
    }
    // query 由 key 决定
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key])

  // 筛选项从全部数据里取（不跟着筛选变少），先查一次不带筛选的。
  const [options, setOptions] = useState<AIOnlineTotal[]>([])
  useEffect(() => {
    serverApi.aiOnline({ from: iso(dayjs().subtract(90, 'day')), to: iso(dayjs()) }).then(
      (d) => setOptions(d.totals),
      () => setOptions([]),
    )
  }, [])
  const labels = useMemo(() => {
    const m = new Map<string, string>()
    for (const t of [...options, ...(data?.totals ?? [])]) m.set(accountKey(t.kind, t.account_id), `${kindTag[t.kind].label} ${t.email}`)
    return m
  }, [options, data])
  const deviceOptions = useMemo(() => {
    const m = new Map<number, string>()
    for (const t of options) m.set(t.device_id, t.device_name || `客户端 ${t.device_id}`)
    return [...m.entries()].map(([value, label]) => ({ value, label }))
  }, [options])
  const accountOptions = useMemo(
    () =>
      [...labels.entries()]
        .filter(([k]) => !kind || k.startsWith(`${kind}:`))
        .map(([value, label]) => ({ value, label }))
        .sort((a, b) => a.label.localeCompare(b.label)),
    [labels, kind],
  )

  const chart = useMemo(() => (data && data.days.length > 0 ? daysOption(data, from, to, labels) : undefined), [data, from, to, labels])
  const total = (data?.totals ?? []).reduce((n, t) => n + t.seconds, 0)

  const kindCell = (k: OnlineKind) => <Tag color={kindTag[k].color}>{kindTag[k].label}</Tag>
  const deviceCell = (_: unknown, r: { device_id: number; device_name: string }) => r.device_name || `客户端 ${r.device_id}`

  return (
    <Space orientation="vertical" size={16} style={{ width: '100%' }}>
      <Card size="small">
        <Space wrap>
          <Segmented<Span> value={span} onChange={setSpan} options={spanOptions} />
          {span === 'custom' && (
            <DatePicker.RangePicker
              allowClear={false}
              value={custom ?? [from, to]}
              disabledDate={(d) => d.isAfter(dayjs(), 'day') || d.isBefore(dayjs().subtract(400, 'day'), 'day')}
              onChange={(v) => v?.[0] && v[1] && setCustom([v[0], v[1]])}
            />
          )}
          <Select
            allowClear
            placeholder="全部客户端"
            style={{ minWidth: 160 }}
            value={device}
            onChange={setDevice}
            options={deviceOptions}
          />
          <Select
            allowClear
            placeholder="全部类型"
            style={{ minWidth: 120 }}
            value={kind}
            onChange={(k) => {
              setKind(k)
              if (k && account && !account.startsWith(`${k}:`)) setAccount(undefined)
            }}
            options={(Object.keys(kindTag) as OnlineKind[]).map((k) => ({ value: k, label: kindTag[k].label }))}
          />
          <Select
            allowClear
            showSearch
            optionFilterProp="label"
            placeholder="全部账号"
            style={{ minWidth: 240 }}
            value={account}
            onChange={setAccount}
            options={accountOptions}
          />
        </Space>
        <div className="hint" style={{ marginTop: 8 }}>
          按切换时间算：Gemini 是客户端的当前账号；Claude / Codex 是有进程在运行的工作区（Windows 拿不到进程数，按设为默认的账号）。
          切到别的账号、进程退出或客户端离线超过 3 分钟时一段结束。
        </div>
      </Card>

      {error && <Alert type="error" showIcon title={error} />}

      <Card
        title={
          <span className="card-title">
            <FieldTimeOutlined />
            每天在线时长
          </span>
        }
        extra={data && <span className="hint">合计 {formatDuration(total)}</span>}
      >
        {!data ? (
          !error && <Skeleton active paragraph={{ rows: 5 }} />
        ) : chart ? (
          <EChart option={chart} height={280} />
        ) : (
          <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="这段时间没有在线记录" />
        )}
      </Card>

      <Card
        title={
          <span className="card-title">
            <FieldTimeOutlined />
            按客户端和账号
          </span>
        }
      >
        <Table<AIOnlineTotal>
          size="small"
          loading={!data && !error}
          rowKey={(r) => `${r.device_id}:${r.kind}:${r.account_id}`}
          dataSource={data?.totals ?? []}
          pagination={{ pageSize: 20, hideOnSinglePage: true }}
          scroll={{ x: 'max-content' }}
          columns={[
            { title: '客户端', dataIndex: 'device_name', render: deviceCell },
            { title: '类型', dataIndex: 'kind', render: kindCell },
            { title: '账号', dataIndex: 'email', render: (v: string) => <span className="selectable">{v}</span> },
            {
              title: '在线时长',
              dataIndex: 'seconds',
              defaultSortOrder: 'descend',
              sorter: (a, b) => a.seconds - b.seconds,
              render: (v: number) => formatDuration(v),
            },
            { title: '段数', dataIndex: 'sessions', sorter: (a, b) => a.sessions - b.sessions },
            {
              title: '状态',
              render: (_, r) =>
                r.online_since ? (
                  <Tag color="success">在用 · {formatClock(r.online_since)} 起</Tag>
                ) : (
                  <span className="hint">最近 {formatClock(r.last_at)}</span>
                ),
            },
          ]}
        />
      </Card>

      <Card
        title={
          <span className="card-title">
            <HistoryOutlined />
            切换记录
          </span>
        }
        extra={data && data.sessions.length >= 500 && <span className="hint">只显示最近 500 段</span>}
      >
        <Table<AIOnlineSession>
          size="small"
          loading={!data && !error}
          rowKey="id"
          dataSource={data?.sessions ?? []}
          pagination={{ pageSize: 20, hideOnSinglePage: true, showSizeChanger: false }}
          scroll={{ x: 'max-content' }}
          columns={[
            { title: '切入', dataIndex: 'start_at', render: (v: string) => formatClock(v) },
            {
              title: '切出',
              dataIndex: 'end_at',
              render: (v: string) => (v ? formatClock(v) : <Tag color="success">在用</Tag>),
            },
            { title: '时长', dataIndex: 'seconds', render: (v: number) => formatDuration(v) },
            { title: '客户端', dataIndex: 'device_name', render: deviceCell },
            { title: '类型', dataIndex: 'kind', render: kindCell },
            { title: '账号', dataIndex: 'email', render: (v: string) => <span className="selectable">{v}</span> },
          ]}
        />
      </Card>
    </Space>
  )
}
