import { useCallback, useEffect, useState } from 'react'
import { App, Button, Card, Empty, Segmented, Select, Skeleton, Table, Tabs, Tag, Tooltip } from 'antd'
import { HistoryOutlined, ReloadOutlined } from '@ant-design/icons'
import { api, type ConfigVersion, type ConfigVersionDetail, type ConfigVersionDiff, type ConfirmState } from '../../api'
import { PageHeader } from '../common'

const states: Record<ConfirmState, { color: string; label: string }> = {
  pending: { color: 'processing', label: '确认中' },
  confirmed: { color: 'success', label: '已确认' },
  unconfirmed: { color: 'warning', label: '未确认（上游故障）' },
  rolled_back: { color: 'error', label: '已回滚' },
  failed: { color: 'error', label: '失败' },
}

const artifacts = [
  { key: 'mihomo', label: '内核配置' },
  { key: 'guard_policy', label: 'guard 策略' },
  { key: 'dnsmasq', label: 'dnsmasq 片段' },
] as const

type Artifact = (typeof artifacts)[number]['key']

const time = (t: string) => new Date(t).toLocaleString()

// 配置版本（M8-04）：内核配置、guard 策略、dnsmasq 片段作为同一个版本，可查看和对比。
export default function Versions() {
  const { message } = App.useApp()
  const [list, setList] = useState<ConfigVersion[]>()
  const [selected, setSelected] = useState<number>()
  const [base, setBase] = useState<number | 'prev'>('prev')
  const [view, setView] = useState<'diff' | 'full'>('diff')
  const [diff, setDiff] = useState<ConfigVersionDiff>()
  const [detail, setDetail] = useState<ConfigVersionDetail>()

  const load = useCallback(() => {
    api.box.versions().then(
      (l) => {
        setList(l)
        setSelected((s) => s ?? l[0]?.id)
      },
      (e: Error) => message.error(e.message),
    )
  }, [message])

  useEffect(() => {
    load()
    const t = setInterval(load, 5000)
    return () => clearInterval(t)
  }, [load])

  useEffect(() => {
    if (selected === undefined) return
    let stale = false
    const req = view === 'diff' ? api.box.versionDiff(selected, base === 'prev' ? undefined : base) : api.box.version(selected)
    req.then(
      (r) => {
        if (stale) return
        if (view === 'diff') setDiff(r as ConfigVersionDiff)
        else setDetail(r as ConfigVersionDetail)
      },
      (e: Error) => !stale && message.error(e.message),
    )
    return () => {
      stale = true
    }
  }, [selected, base, view, message])

  if (!list) {
    return (
      <Card>
        <Skeleton active paragraph={{ rows: 6 }} />
      </Card>
    )
  }

  const shown = view === 'diff' ? (diff?.to === selected ? diff : undefined) : detail?.id === selected ? detail : undefined

  return (
    <div className="desktop-stack">
      <PageHeader
        title="配置版本"
        count={list.length}
        actions={
          <Button icon={<ReloadOutlined />} onClick={load}>
            刷新
          </Button>
        }
      />
      <div className="versions-layout">
        <Card className="versions-list" styles={{ body: { padding: 0 } }}>
          <Table<ConfigVersion>
            rowKey="id"
            size="small"
            pagination={false}
            dataSource={list}
            locale={{ emptyText: <Empty description="内核启动并应用配置后开始记录" /> }}
            rowClassName={(r) => (r.id === selected ? 'ant-table-row-selected' : '')}
            onRow={(r) => ({ onClick: () => setSelected(r.id), style: { cursor: 'pointer' } })}
            columns={[
              {
                title: '版本',
                dataIndex: 'id',
                width: 72,
                render: (id: number, r) => (
                  <span>
                    #{id}
                    {r.current && (
                      <Tag color="purple" className="versions-current">
                        当前
                      </Tag>
                    )}
                  </span>
                ),
              },
              { title: '时间', dataIndex: 'created_at', render: time },
              {
                title: '状态',
                dataIndex: 'status',
                render: (s: ConfirmState, r) => (
                  <Tooltip title={r.reason}>
                    <Tag color={states[s]?.color}>{states[s]?.label ?? s}</Tag>
                  </Tooltip>
                ),
              },
            ]}
          />
        </Card>

        <Card
          title={
            <span className="card-title">
              <HistoryOutlined />
              {selected ? `版本 #${selected}` : '版本'}
            </span>
          }
          extra={
            <div className="versions-controls">
              {view === 'diff' && (
                <Select
                  size="small"
                  value={base}
                  style={{ width: 140 }}
                  onChange={setBase}
                  options={[
                    { value: 'prev', label: '对比上一版' },
                    ...list.filter((v) => v.id !== selected).map((v) => ({ value: v.id, label: `对比 #${v.id}` })),
                  ]}
                />
              )}
              <Segmented
                size="small"
                value={view}
                onChange={(v) => setView(v as 'diff' | 'full')}
                options={[
                  { value: 'diff', label: '差异' },
                  { value: 'full', label: '全文' },
                ]}
              />
            </div>
          }
        >
          {selected === undefined ? (
            <Empty description="还没有版本" />
          ) : !shown ? (
            <Skeleton active paragraph={{ rows: 8 }} />
          ) : (
            <Tabs
              items={artifacts.map((a) => ({
                key: a.key,
                label: a.label,
                children:
                  view === 'diff' ? <DiffView text={(shown as ConfigVersionDiff)[a.key as Artifact]} /> : <pre className="versions-pre">{(shown as ConfigVersionDetail)[a.key as Artifact]}</pre>,
              }))}
            />
          )}
          <p className="hint">内核配置里的密码、UUID、密钥等凭据已打码。三种产物任一变化就记一个新版本，最多保留 30 个。</p>
        </Card>
      </div>
    </div>
  )
}

function DiffView({ text }: { text: string }) {
  if (!text) return <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="与对比版本相同" />
  return (
    <pre className="versions-pre">
      {text.split('\n').map((l, i) => (
        <div key={i} className={l.startsWith('@@') ? 'diff-hunk' : l.startsWith('+') ? 'diff-add' : l.startsWith('-') ? 'diff-del' : undefined}>
          {l || ' '}
        </div>
      ))}
    </pre>
  )
}
