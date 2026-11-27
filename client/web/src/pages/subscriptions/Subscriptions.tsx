import { useCallback, useEffect, useState } from 'react'
import { App, Button, Progress, Space, Table, Tag, Tooltip, Typography } from 'antd'
import { EditOutlined, PlusOutlined, SyncOutlined, WarningOutlined } from '@ant-design/icons'
import {
  api,
  ApiRequestError,
  type Impact,
  type Preview,
  type SubDefaults,
  type Subscription,
  type UserInfo,
} from '../../api'
import SubscriptionForm from './SubscriptionForm'
import PreviewModal from './PreviewModal'
import { PageHeader, RowAction, RowDelete, tableEmpty } from '../common'

function formatBytes(n: number): string {
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024
    i++
  }
  return `${n.toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}

function Usage({ info }: { info: UserInfo | null }) {
  if (!info) return <Typography.Text type="secondary">未知</Typography.Text>
  const used = info.upload + info.download
  const expire = info.expire ? new Date(info.expire * 1000).toLocaleDateString() : '未知'
  return (
    <div style={{ minWidth: 160 }}>
      {info.total > 0 ? (
        <Progress percent={Math.round((used / info.total) * 100)} size="small" />
      ) : null}
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        {formatBytes(used)} / {info.total ? formatBytes(info.total) : '∞'} · 到期 {expire}
      </Typography.Text>
    </div>
  )
}

export default function Subscriptions() {
  const { message, modal } = App.useApp()
  const [list, setList] = useState<Subscription[]>([])
  const [defaults, setDefaults] = useState<SubDefaults>()
  const [loading, setLoading] = useState(true)
  const [formOpen, setFormOpen] = useState(false)
  const [editing, setEditing] = useState<Subscription>()
  const [previewing, setPreviewing] = useState<{ sub: Subscription; preview: Preview }>()
  const [fetchingId, setFetchingId] = useState<string>()

  const reload = useCallback(async () => {
    try {
      setList(await api.subscriptions.list())
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setLoading(false)
    }
  }, [message])

  useEffect(() => {
    const fail = (e: Error) => message.error(e.message)
    api.subscriptions
      .list()
      .then(setList, fail)
      .finally(() => setLoading(false))
    api.subscriptions.defaults().then(setDefaults, fail)
  }, [message])

  const refresh = async (s: Subscription) => {
    setFetchingId(s.id)
    try {
      setPreviewing({ sub: s, preview: await api.subscriptions.preview(s.id) })
    } catch (e) {
      message.error(`更新失败：${(e as Error).message}`)
      reload() // last_error 已更新
    } finally {
      setFetchingId(undefined)
    }
  }

  const openCreate = () => {
    setEditing(undefined)
    setFormOpen(true)
  }

  const remove = async (s: Subscription) => {
    try {
      await api.subscriptions.remove(s.id)
      reload()
    } catch (e) {
      if (e instanceof ApiRequestError && e.body.code === 'references_exist') {
        const impacts = e.body.detail as Impact[]
        modal.confirm({
          title: '该订阅的节点仍被引用',
          content: (
            <ul>
              {impacts.map((i) => (
                <li key={`${i.node_id}-${i.ref_id}`}>
                  {i.node_name} ← {i.ref_name || i.ref_id}
                </li>
              ))}
            </ul>
          ),
          okText: '仍然删除',
          okButtonProps: { danger: true },
          onOk: () => api.subscriptions.remove(s.id, true).then(reload),
        })
        return
      }
      message.error((e as Error).message)
    }
  }

  return (
    <>
      <PageHeader
        title="订阅"
        count={list.length}
        actions={
          <Button type="primary" icon={<PlusOutlined />} disabled={!defaults} onClick={openCreate}>
            添加订阅
          </Button>
        }
      />
      <Table<Subscription>
        rowKey="id"
        size="small"
        dataSource={list}
        pagination={false}
        scroll={{ x: true }}
        locale={{
          emptyText: tableEmpty(
            loading,
            '还没有订阅',
            <Button icon={<PlusOutlined />} disabled={!defaults} onClick={openCreate}>
              添加订阅
            </Button>,
          ),
        }}
        columns={[
          {
            title: '名称',
            render: (_, s) => (
              <Space orientation="vertical" size={0}>
                <span>
                  {s.display_name || s.name} {!s.enabled && <Tag>已禁用</Tag>}
                </span>
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                  {s.name} · {s.fetch_via === 'proxy' ? '经代理' : '直连'}拉取
                </Typography.Text>
              </Space>
            ),
          },
          { title: '节点', dataIndex: 'node_count', width: 70, align: 'right' },
          { title: '流量 / 到期', render: (_, s) => <Usage info={s.userinfo} /> },
          {
            title: '最近更新',
            render: (_, s) =>
              s.last_error ? (
                <Tooltip title={s.last_error}>
                  <Tag color="error" icon={<WarningOutlined />}>
                    失败
                  </Tag>
                </Tooltip>
              ) : s.last_fetch_at ? (
                new Date(s.last_fetch_at).toLocaleString()
              ) : (
                <Typography.Text type="secondary">从未</Typography.Text>
              ),
          },
          {
            title: '操作',
            width: 110,
            align: 'right',
            fixed: 'right',
            render: (_, s) => (
              <div className="row-actions">
                <RowAction title="更新" icon={<SyncOutlined />} loading={fetchingId === s.id} onClick={() => refresh(s)} />
                <RowAction
                  title="编辑"
                  icon={<EditOutlined />}
                  onClick={() => {
                    setEditing(s)
                    setFormOpen(true)
                  }}
                />
                <RowDelete confirm="删除订阅及其全部节点？" onConfirm={() => remove(s)} />
              </div>
            ),
          },
        ]}
      />
      {defaults && formOpen && (
        <SubscriptionForm
          editing={editing}
          defaults={defaults}
          onClose={() => setFormOpen(false)}
          onSaved={() => {
            setFormOpen(false)
            reload()
          }}
        />
      )}
      {previewing && (
        <PreviewModal
          subscription={previewing.sub}
          preview={previewing.preview}
          onClose={() => setPreviewing(undefined)}
          onApplied={() => {
            setPreviewing(undefined)
            message.success('已应用')
            reload()
          }}
        />
      )}
    </>
  )
}
