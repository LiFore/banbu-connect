import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react'
import { App, Button, Dropdown, Input, Select, Space, Switch, Table, Tag, Tooltip, Typography } from 'antd'
import {
  CheckOutlined,
  CloudServerOutlined,
  DeleteOutlined,
  DownOutlined,
  EditOutlined,
  GlobalOutlined,
  ImportOutlined,
  LinkOutlined,
  LoadingOutlined,
  PlusOutlined,
  StopOutlined,
  ThunderboltOutlined,
  ToolOutlined,
} from '@ant-design/icons'
import {
  api,
  ApiRequestError,
  type GroupView,
  type Impact,
  type NodeDetail,
  type ProtocolSchema,
  type ProxyNode,
  type TestResult,
} from '../../api'
import NodeForm from './NodeForm'
import ImportModal from './ImportModal'
import TestResultView from './TestResultView'
import { PageHeader, RowAction, RowDelete, tableEmpty } from '../common'

type DelayState = { loading: true } | { ms: number } | { error: string }

const kindTag: Record<ProxyNode['kind'], { color: string; label: string; icon: ReactNode }> = {
  subscription: { color: 'default', label: '订阅', icon: <LinkOutlined /> },
  manual: { color: 'cyan', label: '自建', icon: <ToolOutlined /> },
  isp: { color: 'gold', label: 'ISP', icon: <CloudServerOutlined /> },
}

const BATCH_CONCURRENCY = 6

function DelayCell({ s }: { s?: DelayState }) {
  if (!s) return <Typography.Text type="secondary">—</Typography.Text>
  if ('loading' in s) return <LoadingOutlined aria-label="测试中" />
  if ('error' in s)
    return (
      <Tooltip title={s.error}>
        <Tag color="error">失败</Tag>
      </Tooltip>
    )
  return <Tag color={s.ms < 300 ? 'success' : s.ms < 800 ? 'warning' : 'error'}>{s.ms} ms</Tag>
}

export default function Nodes() {
  const { message, modal } = App.useApp()
  const [nodes, setNodes] = useState<ProxyNode[]>([])
  const [schemas, setSchemas] = useState<ProtocolSchema[]>([])
  const [groups, setGroups] = useState<GroupView[]>([])
  const [regions, setRegions] = useState<string[]>([])
  const [loading, setLoading] = useState(true)
  const [filters, setFilters] = useState<{ source?: string; region?: string; protocol?: string; q: string }>({ q: '' })
  const [selected, setSelected] = useState<string[]>([])
  const [delays, setDelays] = useState<Record<string, DelayState>>({})
  const [form, setForm] = useState<{ editing?: NodeDetail }>()
  const [importing, setImporting] = useState(false)

  const reload = useCallback(
    () =>
      api.nodes
        .list()
        .then(setNodes, (e: Error) => message.error(e.message))
        .finally(() => setLoading(false)),
    [message],
  )

  useEffect(() => {
    const fail = (e: Error) => message.error(e.message)
    api.nodes
      .list()
      .then(setNodes, fail)
      .finally(() => setLoading(false))
    api.nodes.schemas().then(setSchemas, fail)
    api.groups.list().then(setGroups, fail)
    api.subscriptions.defaults().then((d) => setRegions(d.regions), fail)
  }, [message])

  const sources = useMemo(() => {
    const subs = [...new Set(nodes.filter((n) => n.subscription_name).map((n) => n.subscription_name))]
    return [
      { value: 'kind:manual', label: '自建隧道' },
      { value: 'kind:isp', label: 'ISP 代理' },
      ...subs.map((s) => ({ value: `sub:${s}`, label: `订阅 ${s}` })),
    ]
  }, [nodes])

  const visible = useMemo(() => {
    const q = filters.q.trim().toLowerCase()
    return nodes.filter((n) => {
      if (filters.source) {
        const [t, v] = filters.source.split(':')
        if (t === 'kind' && n.kind !== v) return false
        if (t === 'sub' && n.subscription_name !== v) return false
      }
      if (filters.region && n.region !== filters.region) return false
      if (filters.protocol && n.protocol !== filters.protocol) return false
      if (q && !`${n.display_name} ${n.server} ${n.tags.join(' ')}`.toLowerCase().includes(q)) return false
      return true
    })
  }, [nodes, filters])

  const testDelay = async (ids: string[]) => {
    setDelays((d) => ({ ...d, ...Object.fromEntries(ids.map((id) => [id, { loading: true } as DelayState])) }))
    const queue = [...ids]
    const worker = async () => {
      for (let id = queue.shift(); id; id = queue.shift()) {
        let s: DelayState
        try {
          const r = await api.nodes.test({ kind: 'delay', id })
          s = { ms: r.delay_ms ?? 0 }
        } catch (e) {
          s = { error: (e as Error).message }
        }
        setDelays((d) => ({ ...d, [id as string]: s }))
      }
    }
    await Promise.all(Array.from({ length: Math.min(BATCH_CONCURRENCY, ids.length) }, worker))
  }

  const testExit = async (n: ProxyNode) => {
    const hide = message.loading(`正在检测 ${n.display_name} 的出口…`, 0)
    let view: { ok: boolean; res?: TestResult; msg?: string }
    try {
      view = { ok: true, res: await api.nodes.test({ kind: 'exit', id: n.id }) }
    } catch (e) {
      view = { ok: false, msg: (e as Error).message }
    } finally {
      hide()
    }
    modal.info({ title: n.display_name, width: 560, content: <TestResultView kind="exit" {...view} /> })
  }

  const toggle = async (n: ProxyNode, enabled: boolean) => {
    try {
      await api.nodes.patch(n.id, { enabled })
      setNodes((ns) => ns.map((x) => (x.id === n.id ? { ...x, enabled } : x)))
    } catch (e) {
      message.error((e as Error).message)
    }
  }

  const batchEnable = async (enabled: boolean) => {
    try {
      const r = await api.nodes.batchEnable(selected, enabled)
      message.success(`已${enabled ? '启用' : '禁用'} ${r.updated} 个节点`)
      reload()
    } catch (e) {
      message.error((e as Error).message)
    }
  }

  const batchRemove = () => {
    const ids = selected
    const subs = nodes.filter((n) => ids.includes(n.id) && n.kind === 'subscription').length
    const done = (r: { deleted: number; skipped: number }) => {
      message.success(r.skipped ? `已删除 ${r.deleted} 个节点，跳过 ${r.skipped} 个订阅节点` : `已删除 ${r.deleted} 个节点`)
      setSelected([])
      void reload()
    }
    modal.confirm({
      title: `删除选中的 ${ids.length} 个节点？`,
      content: subs > 0 ? `其中 ${subs} 个订阅节点会跳过：它们会在订阅更新时重新出现，请改为禁用或在订阅里过滤。` : undefined,
      okText: '删除',
      okButtonProps: { danger: true },
      onOk: async () => {
        try {
          done(await api.nodes.batchRemove(ids))
        } catch (e) {
          if (e instanceof ApiRequestError && e.body.code === 'references_exist') {
            const impacts = e.body.detail as Impact[]
            modal.confirm({
              title: '部分节点仍被引用',
              content: (
                <ul>
                  {impacts.map((i) => (
                    <li key={`${i.node_id}-${i.kind}-${i.ref_id}`}>
                      {i.node_name} ← {i.ref_name || i.ref_id}
                    </li>
                  ))}
                </ul>
              ),
              okText: '仍然删除',
              okButtonProps: { danger: true },
              onOk: () => api.nodes.batchRemove(ids, true).then(done, (err: Error) => message.error(err.message)),
            })
            return
          }
          message.error((e as Error).message)
        }
      },
    })
  }

  const edit = async (n: ProxyNode) => {
    try {
      setForm({ editing: await api.nodes.get(n.id) })
    } catch (e) {
      message.error((e as Error).message)
    }
  }

  const remove = async (n: ProxyNode) => {
    try {
      await api.nodes.remove(n.id)
      reload()
    } catch (e) {
      if (e instanceof ApiRequestError && e.body.code === 'references_exist') {
        const impacts = e.body.detail as Impact[]
        modal.confirm({
          title: '该节点仍被引用',
          content: (
            <ul>
              {impacts.map((i) => (
                <li key={`${i.kind}-${i.ref_id}`}>{i.ref_name || i.ref_id}</li>
              ))}
            </ul>
          ),
          okText: '仍然删除',
          okButtonProps: { danger: true },
          onOk: () => api.nodes.remove(n.id, true).then(reload),
        })
        return
      }
      message.error((e as Error).message)
    }
  }

  const protocols = [...new Set(nodes.map((n) => n.protocol))].sort()
  const usedRegions = [...new Set(nodes.map((n) => n.region))].sort()

  return (
    <>
      <PageHeader
        title="节点"
        count={nodes.length}
        actions={
          <Space.Compact>
            <Button type="primary" icon={<PlusOutlined />} disabled={schemas.length === 0} onClick={() => setForm({})}>
              按协议添加
            </Button>
            <Dropdown
              menu={{
                items: [{ key: 'import', icon: <ImportOutlined />, label: '粘贴导入（分享链接 / curl / YAML）' }],
                onClick: () => setImporting(true),
              }}
            >
              <Button type="primary" aria-label="更多添加方式" icon={<DownOutlined />} />
            </Dropdown>
          </Space.Compact>
        }
      />

      <div className="table-toolbar">
        <Select
          allowClear
          placeholder="来源"
          style={{ width: 160 }}
          options={sources}
          value={filters.source}
          onChange={(source) => setFilters((f) => ({ ...f, source }))}
        />
        <Select
          allowClear
          placeholder="地区"
          style={{ width: 120 }}
          options={usedRegions.map((r) => ({ value: r, label: r }))}
          value={filters.region}
          onChange={(region) => setFilters((f) => ({ ...f, region }))}
        />
        <Select
          allowClear
          placeholder="协议"
          style={{ width: 120 }}
          options={protocols.map((p) => ({ value: p, label: p }))}
          value={filters.protocol}
          onChange={(protocol) => setFilters((f) => ({ ...f, protocol }))}
        />
        <Input.Search
          placeholder="名称 / 地址 / 标签"
          allowClear
          style={{ width: 220 }}
          onSearch={(q) => setFilters((f) => ({ ...f, q }))}
        />
        <div className="table-toolbar-end">
          {selected.length > 0 && (
            <>
              <Button icon={<CheckOutlined />} onClick={() => batchEnable(true)}>
                启用选中（{selected.length}）
              </Button>
              <Button icon={<StopOutlined />} onClick={() => batchEnable(false)}>
                禁用选中
              </Button>
              <Button danger icon={<DeleteOutlined />} onClick={batchRemove}>
                删除选中
              </Button>
            </>
          )}
          <Button icon={<ThunderboltOutlined />} disabled={visible.length === 0} onClick={() => testDelay(visible.map((n) => n.id))}>
            全部测速
          </Button>
        </div>
      </div>

      <Table<ProxyNode>
        rowKey="id"
        size="small"
        dataSource={visible}
        locale={{
          emptyText: tableEmpty(loading, nodes.length ? '没有符合条件的节点' : '还没有节点，到「订阅」导入，或点右上角添加'),
        }}
        scroll={{ x: true }}
        pagination={{ defaultPageSize: 50, showSizeChanger: true }}
        rowSelection={{ selectedRowKeys: selected, onChange: (keys) => setSelected(keys as string[]) }}
        columns={[
          {
            title: '名称',
            render: (_, n) => (
              <Space orientation="vertical" size={0} className="node-name">
                <span>{n.display_name}</span>
                {n.dialer_name && (
                  <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                    经 {n.dialer_name}
                  </Typography.Text>
                )}
              </Space>
            ),
          },
          {
            title: '来源',
            render: (_, n) => (
              <Tag color={kindTag[n.kind].color} icon={kindTag[n.kind].icon}>
                {n.kind === 'subscription' ? n.subscription_name : kindTag[n.kind].label}
              </Tag>
            ),
          },
          { title: '地区', dataIndex: 'region', width: 90 },
          { title: '协议', dataIndex: 'protocol', width: 100 },
          {
            title: '标签',
            render: (_, n) => (
              <Space size={[2, 2]} wrap>
                {n.tags.map((t) => (
                  <Tag key={t}>{t}</Tag>
                ))}
              </Space>
            ),
          },
          { title: '延迟', width: 90, render: (_, n) => <DelayCell s={delays[n.id]} /> },
          {
            title: '启用',
            width: 70,
            align: 'center',
            render: (_, n) => <Switch size="small" checked={n.enabled} onChange={(v) => toggle(n, v)} />,
          },
          {
            title: '操作',
            width: 130,
            align: 'right',
            fixed: 'right',
            render: (_, n) => (
              <div className="row-actions">
                <RowAction title="测速" icon={<ThunderboltOutlined />} onClick={() => testDelay([n.id])} />
                <RowAction title="检测出口" icon={<GlobalOutlined />} onClick={() => testExit(n)} />
                <RowAction title="编辑" icon={<EditOutlined />} onClick={() => edit(n)} />
                {n.kind !== 'subscription' && <RowDelete confirm="删除该节点？" onConfirm={() => remove(n)} />}
              </div>
            ),
          },
        ]}
      />

      {form && (
        <NodeForm
          editing={form.editing}
          schemas={schemas}
          regions={regions}
          allNodes={nodes}
          groups={groups}
          onClose={() => setForm(undefined)}
          onSaved={() => {
            setForm(undefined)
            message.success('已保存')
            reload()
          }}
        />
      )}
      {importing && (
        <ImportModal
          allNodes={nodes}
          groups={groups}
          onClose={() => {
            setImporting(false)
            reload()
          }}
          onSaved={() => {
            setImporting(false)
            message.success('已导入')
            reload()
          }}
        />
      )}
    </>
  )
}
