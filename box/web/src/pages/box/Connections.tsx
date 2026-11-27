import { useEffect, useMemo, useRef, useState } from 'react'
import { Alert, App, Card, Empty, Input, Segmented, Select, Space, Table, Tag, Tooltip } from 'antd'
import { ArrowDownOutlined, ArrowUpOutlined, CloseCircleOutlined } from '@ant-design/icons'
import { api, type LiveConn, type LiveFrame, type RuleHit } from '../../api'
import KpiBar from '../desktop/KpiBar'
import { formatBytes, PageHeader, RowAction } from '../common'

const speed = (n: number) => `${formatBytes(n)}/s`

// 实时流量与连接（M11-02）：WebSocket 每秒推送；可按规则、出口组、来源设备筛选。规则命中统计在另一页签。
export default function Connections() {
  const [view, setView] = useState<'conns' | 'hits'>('conns')
  return (
    <div className="desktop-stack">
      <PageHeader
        title="连接"
        actions={
          <Segmented
            value={view}
            onChange={(v) => setView(v as 'conns' | 'hits')}
            options={[
              { value: 'conns', label: '实时连接' },
              { value: 'hits', label: '规则命中' },
            ]}
          />
        }
      />
      {view === 'conns' ? <LiveConnections /> : <RuleHits />}
    </div>
  )
}

function useLive() {
  const [frame, setFrame] = useState<LiveFrame>()
  const [error, setError] = useState<string>()
  const retry = useRef(0)
  useEffect(() => {
    let ws: WebSocket | undefined
    let timer: ReturnType<typeof setTimeout> | undefined
    let closed = false
    const connect = () => {
      ws = new WebSocket(api.box.liveURL())
      ws.onopen = () => {
        retry.current = 0
        setError(undefined)
      }
      ws.onmessage = (e) => setFrame(JSON.parse(e.data as string) as LiveFrame)
      ws.onclose = () => {
        if (closed) return
        setError('实时连接已断开，正在重连…')
        retry.current = Math.min(retry.current + 1, 5)
        timer = setTimeout(connect, retry.current * 1000)
      }
    }
    connect()
    return () => {
      closed = true
      if (timer) clearTimeout(timer)
      ws?.close()
    }
  }, [])
  return { frame, error }
}

function LiveConnections() {
  const { message } = App.useApp()
  const { frame, error } = useLive()
  const [rule, setRule] = useState<string>()
  const [group, setGroup] = useState<string>()
  const [source, setSource] = useState<string>()
  const [query, setQuery] = useState('')

  const conns = useMemo(() => frame?.connections ?? [], [frame])
  const exitOf = (c: LiveConn) => c.chains[c.chains.length - 1] ?? ''
  const options = useMemo(() => {
    const uniq = (xs: string[]) => [...new Set(xs.filter(Boolean))].sort().map((x) => ({ value: x, label: x }))
    return {
      rules: uniq(conns.map((c) => c.rule)),
      groups: uniq(conns.map(exitOf)),
      sources: uniq(conns.map((c) => c.source)),
    }
  }, [conns])
  const shown = conns.filter(
    (c) =>
      (!rule || c.rule === rule) &&
      (!group || exitOf(c) === group) &&
      (!source || c.source === source) &&
      (!query || `${c.host} ${c.destination} ${c.rule_payload}`.toLowerCase().includes(query.toLowerCase())),
  )

  const close = async (id: string) => {
    try {
      await api.box.closeConnection(id)
    } catch (e) {
      message.error((e as Error).message)
    }
  }

  return (
    <>
      {error && <Alert type="warning" showIcon title={error} />}
      {frame && !frame.running && <Alert type="info" showIcon title="内核没有运行，没有连接" />}
      <KpiBar
        items={[
          { label: '上传', value: frame ? speed(frame.up) : '—' },
          { label: '下载', value: frame ? speed(frame.down) : '—' },
          { label: '连接数', value: frame ? String(frame.total) : '—' },
          { label: '累计上传', value: frame ? formatBytes(frame.upload_total) : '—' },
          { label: '累计下载', value: frame ? formatBytes(frame.download_total) : '—' },
        ]}
      />
      <Card styles={{ body: { padding: 0 } }}>
        <Space wrap className="table-toolbar">
          <Select allowClear placeholder="规则" style={{ width: 160 }} value={rule} onChange={setRule} options={options.rules} />
          <Select allowClear placeholder="出口组" style={{ width: 160 }} value={group} onChange={setGroup} options={options.groups} />
          <Select allowClear placeholder="来源设备" style={{ width: 160 }} value={source} onChange={setSource} options={options.sources} />
          <Input.Search allowClear placeholder="域名、地址" style={{ width: 200 }} onChange={(e) => setQuery(e.target.value)} />
          {frame && frame.total > conns.length && <span className="hint">只显示最新的 {conns.length} 条</span>}
        </Space>
        <Table<LiveConn>
          rowKey="id"
          size="small"
          dataSource={shown}
          pagination={{ pageSize: 50, showSizeChanger: false, hideOnSinglePage: true }}
          locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="没有连接" /> }}
          scroll={{ x: 900 }}
          columns={[
            {
              title: '目标',
              render: (_, c) => (
                <Tooltip title={c.destination}>
                  <span className="mono">{c.host || c.destination}</span>
                </Tooltip>
              ),
            },
            { title: '来源', dataIndex: 'source', width: 130, render: (s: string) => <span className="mono">{s}</span> },
            { title: '网络', dataIndex: 'network', width: 64 },
            {
              title: '规则',
              width: 200,
              render: (_, c) => (
                <span>
                  {c.rule}
                  {c.rule_payload && <span className="hint"> {c.rule_payload}</span>}
                </span>
              ),
            },
            {
              title: '出口',
              width: 180,
              render: (_, c) => (
                <Tooltip title={c.chains.slice().reverse().join(' → ')}>
                  <Tag>{exitOf(c)}</Tag>
                  {c.chains.length > 1 && <span className="hint">{c.chains[0]}</span>}
                </Tooltip>
              ),
            },
            {
              title: '流量',
              width: 150,
              render: (_, c) => (
                <span className="hint">
                  <ArrowUpOutlined /> {formatBytes(c.upload)} <ArrowDownOutlined /> {formatBytes(c.download)}
                </span>
              ),
            },
            {
              title: '',
              width: 48,
              render: (_, c) => <RowAction title="断开" icon={<CloseCircleOutlined />} onClick={() => close(c.id)} />,
            },
          ]}
        />
      </Card>
    </>
  )
}

function RuleHits() {
  const { message } = App.useApp()
  const [hits, setHits] = useState<RuleHit[]>()
  useEffect(() => {
    const load = () => api.box.ruleHits().then(setHits, (e: Error) => message.error(e.message))
    load()
    const t = setInterval(load, 5000)
    return () => clearInterval(t)
  }, [message])
  return (
    <Card styles={{ body: { padding: 0 } }}>
      <Table<RuleHit>
        rowKey={(h) => `${h.rule}|${h.payload}`}
        size="small"
        loading={!hits}
        dataSource={hits}
        pagination={{ pageSize: 50, hideOnSinglePage: true }}
        locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有统计" /> }}
        columns={[
          { title: '规则', dataIndex: 'rule', width: 160 },
          { title: '匹配内容', dataIndex: 'payload', render: (p: string) => <span className="mono">{p || '—'}</span> },
          { title: '最近出口', dataIndex: 'target', width: 160, render: (t: string) => (t ? <Tag>{t}</Tag> : '—') },
          { title: '命中次数', dataIndex: 'hits', width: 100, align: 'right' },
        ]}
      />
      <p className="hint table-footnote">按新连接计数，面板启动以来；只统计经过内核的流量。</p>
    </Card>
  )
}
