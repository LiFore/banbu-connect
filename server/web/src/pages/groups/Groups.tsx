import { useCallback, useEffect, useState } from 'react'
import { App, Button, Popover, Space, Table, Tag, Tooltip, Typography } from 'antd'
import {
  AppstoreAddOutlined,
  ArrowDownOutlined,
  ArrowUpOutlined,
  EditOutlined,
  EyeInvisibleOutlined,
  LockOutlined,
  PlusOutlined,
  WarningOutlined,
} from '@ant-design/icons'
import {
  api,
  ApiRequestError,
  type GroupView,
  type Impact,
  type ProxyNode,
  type Subscription,
} from '../../api'
import GroupForm from './GroupForm'
import TemplateModal from './TemplateModal'
import { PageHeader, RowAction, RowDelete, tableEmpty } from '../common'

const typeLabel: Record<GroupView['type'], string> = {
  select: '手动',
  'url-test': '自动',
  fallback: '故障转移',
  'load-balance': '负载均衡',
}

function Members({ g }: { g: GroupView }) {
  const content = (
    <Space size={[4, 4]} wrap style={{ maxWidth: 480 }}>
      {g.resolved.slice(0, 80).map((m) => (
        <Tag key={m.id} color={m.kind === 'group' ? 'purple' : m.kind === 'builtin' ? 'default' : 'cyan'}>
          {m.name}
        </Tag>
      ))}
      {g.resolved.length > 80 && <Tag>…还有 {g.resolved.length - 80} 个</Tag>}
    </Space>
  )
  return (
    <Space size={4}>
      <Popover content={content} title={`${g.name} 的成员`}>
        <a>{g.used_fallback ? `空（兜底 ${g.resolved[0]?.name}）` : `${g.resolved.length} 个`}</a>
      </Popover>
      {g.members.filter.enabled && <Tag>动态</Tag>}
      {g.warnings.length > 0 && (
        <Tooltip title={g.warnings.join('；')}>
          <Tag color="warning" icon={<WarningOutlined />}>
            {g.warnings.length} 个提示
          </Tag>
        </Tooltip>
      )}
    </Space>
  )
}

export default function Groups() {
  const { message, modal } = App.useApp()
  const [groups, setGroups] = useState<GroupView[]>([])
  const [nodes, setNodes] = useState<ProxyNode[]>([])
  const [subs, setSubs] = useState<Subscription[]>([])
  const [loading, setLoading] = useState(true)
  const [form, setForm] = useState<{ editing?: GroupView }>()
  const [templating, setTemplating] = useState(false)

  const loadAll = useCallback(async () => {
    const [g, n, s] = await Promise.all([api.groups.list(), api.nodes.list(), api.subscriptions.list()])
    setGroups(g)
    setNodes(n)
    setSubs(s)
  }, [])

  const reload = useCallback(
    () =>
      loadAll()
        .catch((e: Error) => message.error(e.message))
        .finally(() => setLoading(false)),
    [loadAll, message],
  )

  useEffect(() => {
    Promise.all([api.groups.list(), api.nodes.list(), api.subscriptions.list()])
      .then(([g, n, s]) => {
        setGroups(g)
        setNodes(n)
        setSubs(s)
      })
      .catch((e: Error) => message.error(e.message))
      .finally(() => setLoading(false))
  }, [message])

  const move = async (index: number, delta: number) => {
    const ids = groups.map((g) => g.id)
    const j = index + delta
    if (j < 0 || j >= ids.length) return
    ;[ids[index], ids[j]] = [ids[j], ids[index]]
    try {
      await api.groups.reorder(ids)
      reload()
    } catch (e) {
      message.error((e as Error).message)
    }
  }

  const remove = async (g: GroupView) => {
    try {
      await api.groups.remove(g.id)
      reload()
    } catch (e) {
      if (e instanceof ApiRequestError && e.body.code === 'references_exist') {
        const impacts = e.body.detail as Impact[]
        modal.confirm({
          title: `「${g.name}」仍被引用`,
          content: (
            <>
              <ul>
                {impacts.map((i) => (
                  <li key={`${i.kind}-${i.ref_id}`}>{i.ref_name || i.ref_id}</li>
                ))}
              </ul>
              <Typography.Text type="secondary">
                删除后会从其它组的成员中移除、兜底改为 REJECT、节点前置清空；引用它的规则需要手动调整。
              </Typography.Text>
            </>
          ),
          okText: '仍然删除',
          okButtonProps: { danger: true },
          onOk: () => api.groups.remove(g.id, true).then(reload),
        })
        return
      }
      message.error((e as Error).message)
    }
  }

  const regions = [...new Set(nodes.map((n) => n.region))].sort()

  return (
    <>
      <PageHeader
        title="代理组"
        count={groups.length}
        actions={
          <>
            <Button icon={<AppstoreAddOutlined />} onClick={() => setTemplating(true)}>
              从模板生成
            </Button>
            <Button type="primary" icon={<PlusOutlined />} onClick={() => setForm({})}>
              新建代理组
            </Button>
          </>
        }
      />
      <Table<GroupView>
        rowKey="id"
        size="small"
        dataSource={groups}
        locale={{ emptyText: tableEmpty(loading, '还没有代理组') }}
        pagination={false}
        scroll={{ x: true }}
        columns={[
          {
            title: '顺序',
            width: 80,
            render: (_, _g, i) => (
              <Space size={0}>
                <RowAction title="上移" icon={<ArrowUpOutlined />} disabled={i === 0} onClick={() => move(i, -1)} />
                <RowAction title="下移" icon={<ArrowDownOutlined />} disabled={i === groups.length - 1} onClick={() => move(i, 1)} />
              </Space>
            ),
          },
          {
            title: '名称',
            render: (_, g) => (
              <Space size={4}>
                {g.name}
                {g.strict && (
                  <Tooltip title="严格组：任何路径都不会走 DIRECT">
                    <Tag color="red" icon={<LockOutlined />}>
                      严格
                    </Tag>
                  </Tooltip>
                )}
                {g.options.hidden && <Tag icon={<EyeInvisibleOutlined />}>隐藏</Tag>}
              </Space>
            ),
          },
          { title: '类型', width: 90, render: (_, g) => typeLabel[g.type] },
          { title: '成员', render: (_, g) => <Members g={g} /> },
          {
            title: '操作',
            width: 80,
            align: 'right',
            fixed: 'right',
            render: (_, g) => (
              <div className="row-actions">
                <RowAction title="编辑" icon={<EditOutlined />} onClick={() => setForm({ editing: g })} />
                <RowDelete confirm="删除该代理组？" onConfirm={() => remove(g)} />
              </div>
            ),
          },
        ]}
      />
      {form && (
        <GroupForm
          editing={form.editing}
          groups={groups}
          nodes={nodes}
          subscriptions={subs}
          onClose={() => setForm(undefined)}
          onSaved={() => {
            setForm(undefined)
            message.success('已保存')
            reload()
          }}
        />
      )}
      {templating && (
        <TemplateModal
          regions={regions}
          onClose={() => setTemplating(false)}
          onDone={(msg) => {
            setTemplating(false)
            message.success(msg)
            reload()
          }}
        />
      )}
    </>
  )
}
