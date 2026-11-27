import { useCallback, useEffect, useState } from 'react'
import { Alert, App, Button, Card, Descriptions, Input, Space, Table, Tag } from 'antd'
import { CloudUploadOutlined } from '@ant-design/icons'
import { serverApi, type Draft, type ProfileMeta, type ProfileSummary } from '../../serverApi'
import { PageHeader, tableEmpty } from '../common'

function Summary({ p }: { p: ProfileSummary }) {
  const selections = Object.entries(p.selections ?? {})
  return (
    <Descriptions size="small" column={1}>
      <Descriptions.Item label="节点">{p.nodes.length} 个</Descriptions.Item>
      <Descriptions.Item label="代理组">
        <Space size={[4, 4]} wrap>
          {p.groups.map((g) => (
            <Tag key={g.name}>
              {g.name}（{g.members.length}）
            </Tag>
          ))}
        </Space>
      </Descriptions.Item>
      <Descriptions.Item label="线路">
        {selections.length === 0 ? '未指定' : selections.map(([g, m]) => `${g} → ${m}`).join('；')}
      </Descriptions.Item>
      <Descriptions.Item label="规则">{p.rules} 条</Descriptions.Item>
      <Descriptions.Item label="远程规则集">{p.rule_sets < 0 ? '客户端内置默认' : `${p.rule_sets} 条`}</Descriptions.Item>
      <Descriptions.Item label="默认方案版本">{p.profile_version ? <code>{p.profile_version}</code> : '—'}</Descriptions.Item>
      <Descriptions.Item label="其他方案">
        {(p.variants ?? []).length === 0 ? (
          '无，所有客户端用默认方案'
        ) : (
          <Space size={[4, 4]} wrap>
            {(p.variants ?? []).map((v) => (
              <Tag key={v.name}>
                {v.name}
                {v.version && <code className="tag-code">{v.version}</code>}：{v.devices} 台 · {v.nodes} 节点 · {v.groups} 代理组 · {v.rules} 条规则 ·{' '}
                {v.rule_sets < 0 ? '内置规则集' : `${v.rule_sets} 个规则集`}
              </Tag>
            ))}
          </Space>
        )}
      </Descriptions.Item>
    </Descriptions>
  )
}

// 发布：把服务端当前的节点、代理组、线路和规则发布成新版本，客户端几分钟内同步。
export default function Publish() {
  const { message, modal } = App.useApp()
  const [draft, setDraft] = useState<Draft>()
  const [history, setHistory] = useState<ProfileMeta[]>([])
  const [loading, setLoading] = useState(true)
  const [note, setNote] = useState('')
  const [publishing, setPublishing] = useState(false)

  const reload = useCallback(
    () =>
      Promise.all([serverApi.draft(), serverApi.profiles()])
        .then(([d, h]) => {
          setDraft(d)
          setHistory(h)
        })
        .catch((e: Error) => message.error(e.message))
        .finally(() => setLoading(false)),
    [message],
  )

  useEffect(() => {
    void reload()
  }, [reload])

  const publish = () =>
    modal.confirm({
      title: '发布新版本',
      content: '所有绑定的客户端会在几分钟内同步，节点凭据会下发到这些客户端。',
      okText: '发布',
      cancelText: '取消',
      onOk: async () => {
        setPublishing(true)
        try {
          const p = await serverApi.publish(note)
          message.success(`已发布第 ${p.version} 版`)
          setNote('')
          await reload()
        } catch (e) {
          message.error((e as Error).message)
        } finally {
          setPublishing(false)
        }
      },
    })

  return (
    <Space orientation="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader title="发布" />
      {draft && !draft.changed && <Alert type="success" showIcon title="已发布的版本与当前配置一致" />}
      {draft?.changed && <Alert type="info" showIcon title="当前配置有未发布的修改" />}
      <div className="desktop-columns">
        <Card title="待发布" loading={loading}>
          {draft && <Summary p={draft.draft} />}
          <Space.Compact style={{ width: '100%', marginTop: 8 }}>
            <Input placeholder="说明（可选）" value={note} maxLength={200} onChange={(e) => setNote(e.target.value)} />
            <Button type="primary" icon={<CloudUploadOutlined />} loading={publishing} disabled={!draft} onClick={publish}>
              发布
            </Button>
          </Space.Compact>
        </Card>
        <Card title={draft?.current ? `已发布：第 ${draft.current.version} 版` : '已发布'} loading={loading}>
          {draft?.current ? <Summary p={draft.current} /> : '还没有发布过。'}
        </Card>
      </div>
      <Card title="发布记录">
        <Table<ProfileMeta>
          rowKey="version"
          size="small"
          dataSource={history}
          pagination={false}
          locale={{ emptyText: tableEmpty(loading, '还没有发布过') }}
          columns={[
            { title: '版本', dataIndex: 'version', width: 80 },
            { title: '说明', dataIndex: 'note', render: (v: string) => v || '—' },
            { title: '发布时间', dataIndex: 'created_at', width: 200, render: (v: string) => new Date(v).toLocaleString() },
            { title: '已同步的客户端', dataIndex: 'devices', width: 140 },
          ]}
        />
      </Card>
    </Space>
  )
}
