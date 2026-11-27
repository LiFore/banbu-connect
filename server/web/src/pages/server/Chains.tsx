import { type ReactNode, useCallback, useEffect, useMemo, useState } from 'react'
import { Alert, App, Button, Card, Empty, Modal, Segmented, Select, type SelectProps, Space, Table, Tag } from 'antd'
import { ArrowRightOutlined, DeleteOutlined, EditOutlined, NodeIndexOutlined, PlusOutlined } from '@ant-design/icons'
import { api, type GroupView, type ProxyNode } from '../../api'
import { PageHeader, RowAction } from '../common'

// 链式代理（M6-01、M6-03，ADR-038）：入口 → 中间跳 → 出口。用节点的「前置代理」（dialer-proxy）串起来：
// 出口节点的前置是最后一跳，每一跳的前置是它前面一跳；入口可以是节点或代理组。保存后到「发布」下发。

interface Hop {
  id: string
  name: string
  kind: 'node' | 'group'
}

interface Chain {
  exit: ProxyNode
  hops: Hop[] // 从入口到出口前一跳
  broken?: string
}

function buildChains(nodes: ProxyNode[], groups: GroupView[]): Chain[] {
  const byId = new Map(nodes.map((n) => [n.id, n]))
  const groupById = new Map(groups.map((g) => [g.id, g]))
  const referenced = new Set(nodes.map((n) => n.dialer_ref).filter(Boolean))
  const out: Chain[] = []
  for (const n of nodes) {
    if (!n.dialer_ref || referenced.has(n.id)) continue // 只从链路末端（出口）开始
    const hops: Hop[] = []
    let ref = n.dialer_ref
    let broken: string | undefined
    const seen = new Set([n.id])
    while (ref) {
      if (seen.has(ref)) {
        broken = '有环路'
        break
      }
      seen.add(ref)
      const node = byId.get(ref)
      if (node) {
        hops.unshift({ id: node.id, name: node.display_name, kind: 'node' })
        ref = node.dialer_ref
        continue
      }
      const g = groupById.get(ref)
      if (g) hops.unshift({ id: g.id, name: g.name, kind: 'group' })
      else broken = '前置代理不存在'
      break
    }
    out.push({ exit: n, hops, broken })
  }
  return out.sort((a, b) => a.exit.display_name.localeCompare(b.exit.display_name))
}

// switcher：合并页（Routing）放在页头操作区最前面的「线路 / 链式代理」切换。
export default function Chains({ switcher }: { switcher?: ReactNode }) {
  const { message, modal } = App.useApp()
  const [nodes, setNodes] = useState<ProxyNode[]>()
  const [groups, setGroups] = useState<GroupView[]>([])
  const [view, setView] = useState<'list' | 'graph'>('list')
  const [editing, setEditing] = useState<Chain | 'new'>()

  const load = useCallback(() => {
    Promise.all([api.nodes.list(), api.groups.list()]).then(([n, g]) => {
      setNodes(n)
      setGroups(g)
    }, (e: Error) => message.error(e.message))
  }, [message])

  useEffect(load, [load])

  const chains = useMemo(() => buildChains(nodes ?? [], groups), [nodes, groups])

  const remove = (c: Chain) =>
    modal.confirm({
      title: `拆除「${c.exit.display_name}」的链路？`,
      content: '只清除出口节点的前置代理；中间节点保持不变。',
      onOk: async () => {
        await api.nodes.patch(c.exit.id, { dialer_ref: '' })
        load()
      },
    })

  return (
    <div className="desktop-stack">
      <PageHeader
        title="链式代理"
        count={chains.length}
        actions={
          <Space>
            {switcher}
            <Segmented
              value={view}
              onChange={(v) => setView(v as 'list' | 'graph')}
              options={[
                { value: 'list', label: '列表' },
                { value: 'graph', label: '拓扑' },
              ]}
            />
            <Button type="primary" icon={<PlusOutlined />} onClick={() => setEditing('new')}>
              新建链路
            </Button>
          </Space>
        }
      />
      <Alert
        type="info"
        showIcon
        title="链路由节点的前置代理串成：流量先到入口，再逐跳转发，最后从出口节点出去。保存后到「发布」页发布，客户端才会收到；出口检测得到的应是出口节点的 IP。"
      />
      {view === 'list' ? (
        <Card styles={{ body: { padding: 0 } }}>
          <Table<Chain>
            rowKey={(c) => c.exit.id}
            size="small"
            loading={!nodes}
            pagination={false}
            dataSource={chains}
            locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有链路" /> }}
            columns={[
              {
                title: '链路（入口 → 出口）',
                render: (_, c) => (
                  <span className="chain-path">
                    {c.hops.map((h) => (
                      <span key={h.id}>
                        <Tag color={h.kind === 'group' ? 'purple' : 'blue'}>{h.name}</Tag>
                        <ArrowRightOutlined className="chain-arrow" />
                      </span>
                    ))}
                    <Tag color="green">{c.exit.display_name}</Tag>
                    {c.broken && <Tag color="error">{c.broken}</Tag>}
                  </span>
                ),
              },
              { title: '跳数', width: 70, render: (_, c) => c.hops.length + 1 },
              { title: '出口', width: 160, render: (_, c) => <span className="mono">{c.exit.server}</span> },
              {
                title: '',
                width: 80,
                render: (_, c) => (
                  <div className="row-actions">
                    <RowAction title="编辑" icon={<EditOutlined />} onClick={() => setEditing(c)} />
                    <RowAction title="拆除" icon={<DeleteOutlined />} onClick={() => remove(c)} />
                  </div>
                ),
              },
            ]}
          />
        </Card>
      ) : (
        <Card>{chains.length ? <ChainGraph chains={chains} /> : <Empty description="还没有链路" />}</Card>
      )}
      {editing && nodes && (
        <ChainForm
          chain={editing === 'new' ? undefined : editing}
          nodes={nodes}
          groups={groups}
          onClose={() => setEditing(undefined)}
          onSaved={() => {
            setEditing(undefined)
            message.success('链路已保存到草稿，发布后生效')
            load()
          }}
        />
      )}
    </div>
  )
}

function ChainForm({
  chain,
  nodes,
  groups,
  onClose,
  onSaved,
}: {
  chain?: Chain
  nodes: ProxyNode[]
  groups: GroupView[]
  onClose: () => void
  onSaved: () => void
}) {
  const { message } = App.useApp()
  const [exit, setExit] = useState(chain?.exit.id)
  const [hops, setHops] = useState<string[]>(chain?.hops.map((h) => h.id) ?? [''])
  const [saving, setSaving] = useState(false)

  const nodeOpts = nodes.map((n) => ({ value: n.id, label: `${n.display_name}（${n.server}）` }))
  const groupOpts = groups.map((g) => ({ value: g.id, label: `${g.name}（代理组）` }))
  const isGroup = (id: string) => groups.some((g) => g.id === id)

  const error = (() => {
    if (!exit) return '请选择出口节点'
    if (hops.some((h) => !h)) return '每一跳都要选择'
    const all = [...hops, exit]
    if (new Set(all).size !== all.length) return '同一个节点不能出现两次'
    if (hops.slice(1).some(isGroup)) return '只有入口可以是代理组：代理组不能再设置前置代理'
    return undefined
  })()

  const save = async () => {
    if (error || !exit) return
    setSaving(true)
    try {
      // 从入口往出口设置：入口清空前置，每一跳指向前一跳，出口指向最后一跳。
      if (!isGroup(hops[0])) await api.nodes.patch(hops[0], { dialer_ref: '' })
      for (let i = 1; i < hops.length; i++) await api.nodes.patch(hops[i], { dialer_ref: hops[i - 1] })
      await api.nodes.patch(exit, { dialer_ref: hops[hops.length - 1] })
      onSaved()
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <Modal
      open
      width={620}
      title={chain ? `编辑链路：${chain.exit.display_name}` : '新建链路'}
      onCancel={onClose}
      onOk={save}
      okText="保存"
      okButtonProps={{ disabled: !!error }}
      confirmLoading={saving}
      destroyOnHidden
    >
      <div className="chain-form">
        {hops.map((h, i) => (
          <div key={i} className="chain-form-row">
            <span className="chain-form-label">{i === 0 ? '入口' : `中间跳 ${i}`}</span>
            <Select
              showSearch
              optionFilterProp="label"
              style={{ flex: 1 }}
              placeholder="选择节点"
              value={h || undefined}
              options={(i === 0 ? [{ label: '节点', options: nodeOpts }, { label: '代理组', options: groupOpts }] : nodeOpts) as SelectProps['options']}
              onChange={(v: string) => setHops(hops.map((x, j) => (j === i ? v : x)))}
            />
            {hops.length > 1 && <RowAction title="删除这一跳" icon={<DeleteOutlined />} onClick={() => setHops(hops.filter((_, j) => j !== i))} />}
          </div>
        ))}
        <Button size="small" icon={<PlusOutlined />} onClick={() => setHops([...hops, ''])}>
          添加中间跳
        </Button>
        <div className="chain-form-row">
          <span className="chain-form-label">出口</span>
          <Select showSearch optionFilterProp="label" style={{ flex: 1 }} placeholder="选择出口节点" value={exit} options={nodeOpts} onChange={setExit} disabled={!!chain} />
        </div>
        {error && <Alert type="warning" showIcon title={error} />}
        <p className="hint">保存会改写这些节点的前置代理；某个节点原来属于别的链路时，那条链路也会跟着变。</p>
      </div>
    </Modal>
  )
}

// 拓扑图（M6-03）：按跳的位置分列，入口在左、出口在右，同一个节点在多条链路里只画一次。
function ChainGraph({ chains }: { chains: Chain[] }) {
  const W = 170
  const H = 34
  const GX = 60
  const GY = 18
  const cols: string[][] = []
  const label = new Map<string, Hop | { id: string; name: string; kind: 'exit' }>()
  const place = (id: string, col: number) => {
    for (const c of cols) if (c.includes(id)) return
    while (cols.length <= col) cols.push([])
    cols[col].push(id)
  }
  const maxLen = Math.max(...chains.map((c) => c.hops.length))
  const edges: [string, string][] = []
  for (const c of chains) {
    const offset = maxLen - c.hops.length // 出口对齐在最右列
    c.hops.forEach((h, i) => {
      label.set(h.id, h)
      place(h.id, offset + i)
    })
    label.set(c.exit.id, { id: c.exit.id, name: c.exit.display_name, kind: 'exit' })
    place(c.exit.id, maxLen)
    const path = [...c.hops.map((h) => h.id), c.exit.id]
    for (let i = 0; i + 1 < path.length; i++) edges.push([path[i], path[i + 1]])
  }
  const pos = new Map<string, { x: number; y: number }>()
  cols.forEach((col, ci) => col.forEach((id, ri) => pos.set(id, { x: ci * (W + GX), y: ri * (H + GY) })))
  const width = cols.length * (W + GX) - GX
  const height = Math.max(...cols.map((c) => c.length)) * (H + GY) - GY
  return (
    <div className="chain-graph">
      <svg width={width} height={height} viewBox={`0 0 ${width} ${height}`} role="img" aria-label="链式代理拓扑">
        {edges.map(([a, b], i) => {
          const p = pos.get(a)!
          const q = pos.get(b)!
          return <path key={i} d={`M${p.x + W},${p.y + H / 2} C${p.x + W + GX / 2},${p.y + H / 2} ${q.x - GX / 2},${q.y + H / 2} ${q.x},${q.y + H / 2}`} className="chain-edge" />
        })}
        {[...pos.entries()].map(([id, p]) => {
          const l = label.get(id)!
          return (
            <g key={id} transform={`translate(${p.x},${p.y})`}>
              <rect width={W} height={H} rx={6} className={`chain-node ${l.kind}`} />
              <text x={W / 2} y={H / 2 + 4} textAnchor="middle" className="chain-text">
                {l.name.length > 14 ? `${l.name.slice(0, 13)}…` : l.name}
              </text>
            </g>
          )
        })}
      </svg>
      <div className="chain-legend">
        <Tag color="purple">代理组</Tag>
        <Tag color="blue">节点</Tag>
        <Tag color="green">出口</Tag>
        <NodeIndexOutlined /> 同一个节点只画一次
      </div>
    </div>
  )
}
