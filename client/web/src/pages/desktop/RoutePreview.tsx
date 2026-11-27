import { useState } from 'react'
import { App, Button, Card, Empty, Input, Space, Table, Tag, Tooltip } from 'antd'
import { DeleteOutlined, NodeIndexOutlined, ReloadOutlined, RightOutlined } from '@ant-design/icons'
import { api, type Egress, type RouteTest } from '../../api'
import CountryFlag, { countryName } from './CountryFlag'

// 线路预览（高级）：输入域名，经内核真实请求一次，看命中的规则、规则选中的代理组 → 实际节点、
// 这条线路的出口（国旗、IP）和延迟（新建连接、已建立连接上的往返）。和管理服务「规则预览」的离线模拟不同，
// 这里是实际结果：远程规则集、GEOIP、当前选中的节点都算在内（见 internal/desktop/routetest.go）。

const presets = ['google.com', 'github.com', 'claude.ai', 'chatgpt.com', 'api.anthropic.com', 'gemini.google.com', 'baidu.com']

const latencyColor = (ms?: number) => (!ms ? 'default' : ms < 300 ? 'green' : ms < 800 ? 'orange' : 'red')

export function EgressText({ eg }: { eg?: Egress }) {
  if (!eg) return <span className="hint">—</span>
  if (!eg.ok)
    return (
      <Tooltip title={eg.error}>
        <Tag color="red">{eg.error && eg.error.includes('没有可用的探测入口') ? '无法探测' : '失败'}</Tag>
      </Tooltip>
    )
  return (
    <span className="egress-inline selectable">
      <CountryFlag code={eg.country} />
      <span className="mono-input">{eg.ip}</span>
      {eg.country && <span className="hint">{countryName(eg.country)}</span>}
    </span>
  )
}

export function RoutePath({ r }: { r: RouteTest }) {
  if (!r.path?.length) return <span className="hint">—</span>
  return (
    <span className="route-path">
      {r.path.map((p, i) => (
        <span key={`${i}-${p}`} className="route-path-item">
          {i > 0 && <RightOutlined className="route-path-sep" />}
          <Tag color={i === 0 ? 'blue' : i === r.path!.length - 1 ? 'purple' : undefined}>{p}</Tag>
        </span>
      ))}
    </span>
  )
}

export function LatencyText({ r }: { r: RouteTest }) {
  if (!r.ok) return <span className="hint">—</span>
  return (
    <Tooltip title={`新建连接（经代理建立隧道 + TLS 握手）${r.connect_ms ?? '—'} ms；已建立连接上 ${r.samples?.length ?? 0} 次请求：${(r.samples ?? []).join('、')} ms`}>
      <Space size={4}>
        <Tag color={latencyColor(r.latency_ms)}>{r.latency_ms} ms</Tag>
        <span className="hint">建连 {r.connect_ms} ms</span>
      </Space>
    </Tooltip>
  )
}

export default function RoutePreview() {
  const { message } = App.useApp()
  const [target, setTarget] = useState('')
  const [results, setResults] = useState<RouteTest[]>([])
  const [running, setRunning] = useState<string[]>([])

  const test = async (t: string) => {
    t = t.trim()
    if (!t || running.includes(t)) return
    setRunning((x) => [...x, t])
    try {
      const r = await api.network.testRoute(t)
      setResults((x) => [r, ...x.filter((y) => y.target !== t)])
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setRunning((x) => x.filter((y) => y !== t))
    }
  }

  return (
    <div className="desktop-stack">
      <Card
        title={
          <span className="card-title">
            <NodeIndexOutlined />
            线路预览
          </span>
        }
      >
        <p className="hint">
          输入域名或地址，经本机代理按规则真实请求一次：显示命中的规则、规则选中的代理组和实际节点，以及这条线路的出口和延迟。远程规则集、GEOIP 和当前选中的节点都算在内。要先启动代理。
        </p>
        <Space.Compact style={{ width: '100%', maxWidth: 560 }}>
          <Input
            placeholder="域名或地址，如 github.com、https://claude.ai/"
            value={target}
            spellCheck={false}
            onChange={(e) => setTarget(e.target.value)}
            onPressEnter={() => void test(target)}
            allowClear
          />
          <Button type="primary" loading={running.includes(target.trim())} onClick={() => void test(target)}>
            测试
          </Button>
        </Space.Compact>
        <div className="route-presets">
          <span className="hint">常用：</span>
          {presets.map((p) => (
            <Tag.CheckableTag key={p} checked={false} onChange={() => void test(p)}>
              {running.includes(p) ? `${p}…` : p}
            </Tag.CheckableTag>
          ))}
        </div>
      </Card>

      <Card size="small">
        {results.length === 0 && running.length === 0 ? (
          <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有测试结果" />
        ) : (
          <Table<RouteTest>
            size="small"
            rowKey="target"
            pagination={false}
            scroll={{ x: 'max-content' }}
            dataSource={results}
            loading={running.length > 0 && results.length === 0}
            columns={[
              {
                title: '域名',
                key: 'host',
                fixed: 'left',
                render: (_, r) => (
                  <div className="selectable">
                    {r.host || r.target}
                    {r.status ? <span className="hint"> · HTTP {r.status}</span> : null}
                    {r.error && <div className="hint route-error">{r.error}</div>}
                  </div>
                ),
              },
              {
                title: '命中规则',
                key: 'rule',
                render: (_, r) =>
                  r.rule ? (
                    <span className="selectable">
                      <code>{r.rule}</code>
                      {r.rule_payload && <span className="hint"> {r.rule_payload}</span>}
                    </span>
                  ) : (
                    <span className="hint">—</span>
                  ),
              },
              { title: '线路', key: 'path', render: (_, r) => <RoutePath r={r} /> },
              { title: '出口', key: 'egress', render: (_, r) => <EgressText eg={r.egress} /> },
              {
                title: '延迟',
                key: 'latency',
                render: (_, r) =>
                  r.error && !r.ok ? (
                    <Tooltip title={r.error}>
                      <Tag color="red">连不上</Tag>
                    </Tooltip>
                  ) : (
                    <LatencyText r={r} />
                  ),
              },
              {
                title: '',
                key: 'ops',
                width: 72,
                render: (_, r) => (
                  <Space size={4}>
                    <Tooltip title="重新测试">
                      <Button size="small" type="text" icon={<ReloadOutlined />} loading={running.includes(r.target)} onClick={() => void test(r.target)} />
                    </Tooltip>
                    <Tooltip title="移除">
                      <Button size="small" type="text" icon={<DeleteOutlined />} onClick={() => setResults((x) => x.filter((y) => y.target !== r.target))} />
                    </Tooltip>
                  </Space>
                ),
              },
            ]}
          />
        )}
      </Card>
    </div>
  )
}
