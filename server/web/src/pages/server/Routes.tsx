import { type ReactNode, useCallback, useEffect, useState } from 'react'
import { Alert, App, Button, Card, Empty, Input, Skeleton, Space, Table, Tag, Typography } from 'antd'
import { EditOutlined, PlusOutlined, SaveOutlined } from '@ant-design/icons'
import { Link } from 'react-router-dom'
import { api, type GroupView, type ProxyNode, type Subscription } from '../../api'
import { serverApi } from '../../serverApi'
import { PageHeader, RowAction } from '../common'
import GroupForm from '../groups/GroupForm'

// 线路：线路是手动选择类的代理组，是整个系统的；在这里新增、编辑，立即保存到草稿。严格线路允许的出口所有客户端一致。
// 每个方案拥有哪些线路、默认选哪个成员、规则和远程规则集，在「方案」页配置（ADR-064）。都要到「发布」后客户端才收到。

// switcher：合并页（Routing）放在页头操作区最前面的「线路 / 链式代理」切换。
export default function Routes({ switcher }: { switcher?: ReactNode }) {
  const { message } = App.useApp()
  const [groupViews, setGroupViews] = useState<GroupView[]>()
  const [nodes, setNodes] = useState<ProxyNode[]>([])
  const [subs, setSubs] = useState<Subscription[]>([])
  const [egress, setEgress] = useState('')
  const [boxSecret, setBoxSecret] = useState('')
  const [dirty, setDirty] = useState(false)
  const [saving, setSaving] = useState(false)
  const [form, setForm] = useState<{ editing?: GroupView }>()

  const loadGroups = useCallback(
    () =>
      Promise.all([api.groups.list(), api.nodes.list(), api.subscriptions.list()])
        .then(([g, n, s]) => {
          setGroupViews(g)
          setNodes(n)
          setSubs(s)
        })
        .catch((e: Error) => message.error(e.message)),
    [message],
  )
  const loadSettings = useCallback(
    () =>
      serverApi
        .settings()
        .then((s) => {
          setEgress((s.strict_egress ?? []).join('\n'))
          setBoxSecret(s.box_secret ?? '')
          setDirty(false)
        })
        .catch((e: Error) => message.error(e.message)),
    [message],
  )

  useEffect(() => {
    void loadGroups()
    void loadSettings()
  }, [loadGroups, loadSettings])

  const lines = (groupViews ?? []).filter((g) => g.type === 'select')

  const save = async () => {
    setSaving(true)
    try {
      await serverApi.updateSettings({ strict_egress: egress.split(/\r?\n/), box_secret: boxSecret.trim() })
      message.success('已保存，到「发布」页发布后客户端才会收到')
      await loadSettings()
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <Space orientation="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader
        title="线路"
        actions={
          <Space>
            {switcher}
            <Button type="primary" icon={<SaveOutlined />} disabled={!dirty} loading={saving} onClick={save}>
              保存
            </Button>
          </Space>
        }
      />
      <Alert
        type="info"
        showIcon
        title="线路是整个系统的"
        description={
          <>
            线路（手动选择类代理组）的新增和编辑立即保存到草稿；严格线路的出口点右上角「保存」。每个方案拥有哪些线路、默认成员、规则和远程规则集，到
            <Link to="/profiles"> 方案 </Link>
            页配置。都要到「发布」后客户端才收到。
          </>
        }
      />
      <Card
        title="线路（手动选择类代理组）"
        extra={
          <Button icon={<PlusOutlined />} onClick={() => setForm({})}>
            新增线路
          </Button>
        }
      >
        {!groupViews ? (
          <Skeleton active paragraph={{ rows: 3 }} />
        ) : lines.length === 0 ? (
          <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有线路，点「新增线路」添加" />
        ) : (
          <Table<GroupView>
            rowKey="name"
            size="small"
            pagination={false}
            dataSource={lines}
            columns={[
              {
                title: '名称',
                dataIndex: 'name',
                render: (v: string, g) => (
                  <Space>
                    {v}
                    {g.strict && <Tag color="orange">严格</Tag>}
                  </Space>
                ),
              },
              {
                title: '成员',
                render: (_, g) => (
                  <Typography.Text type="secondary" ellipsis={{ tooltip: g.resolved.map((m) => m.name).join('、') }} style={{ maxWidth: 520 }}>
                    {g.resolved.length} 个：{g.resolved.map((m) => m.name).join('、')}
                  </Typography.Text>
                ),
              },
              {
                title: '操作',
                width: 70,
                render: (_, g) => <RowAction title="编辑线路" icon={<EditOutlined />} onClick={() => setForm({ editing: g })} />,
              },
            ]}
          />
        )}
      </Card>
      <Card title="严格线路允许的出口（每行一个 IP 或网段，所有客户端一致）">
        <Typography.Paragraph type="secondary">
          客户端经每个严格代理组探测出口，任何一组不在列表里，保护服务就阻断受保护的服务，直到全部符合。留空则只展示探测结果、不阻断。
        </Typography.Paragraph>
        <Input.TextArea
          className="mono-input"
          autoSize={{ minRows: 3, maxRows: 12 }}
          spellCheck={false}
          placeholder={'203.0.113.7\n198.51.100.0/24'}
          value={egress}
          onChange={(e) => {
            setEgress(e.target.value)
            setDirty(true)
          }}
        />
      </Card>
      <Card title="路由盒自动发现">
        <Typography.Paragraph type="secondary">
          客户端在局域网里查找 banbu-box.lan，用这个口令验证路由盒；验证通过、出口确认无误时，除直连外的流量交给路由盒出口，
          受保护的服务仍由出口闸门逐个确认（客户端需打开「闸门直连」）。绑定了管理服务的路由盒自动使用这个口令；没绑定的在路由盒上设置
          <Typography.Text code>uci set boxctl.panel.discovery_secret=…</Typography.Text>。留空表示不启用。发布后生效。
        </Typography.Paragraph>
        <Space.Compact style={{ width: '100%', maxWidth: 520 }}>
          <Input.Password
            value={boxSecret}
            placeholder="12～128 个字符"
            autoComplete="new-password"
            spellCheck={false}
            onChange={(e) => {
              setBoxSecret(e.target.value)
              setDirty(true)
            }}
          />
          <Button
            onClick={() => {
              const b = new Uint8Array(18)
              crypto.getRandomValues(b)
              setBoxSecret(btoa(String.fromCharCode(...b)).replace(/[+/=]/g, ''))
              setDirty(true)
            }}
          >
            随机生成
          </Button>
        </Space.Compact>
      </Card>
      {form && (
        <GroupForm
          editing={form.editing}
          groups={groupViews ?? []}
          nodes={nodes}
          subscriptions={subs}
          onClose={() => setForm(undefined)}
          onSaved={() => {
            setForm(undefined)
            message.success('线路已保存到草稿')
            void loadGroups()
          }}
        />
      )}
    </Space>
  )
}
