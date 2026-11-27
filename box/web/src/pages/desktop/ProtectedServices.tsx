import { useCallback, useEffect, useState } from 'react'
import { App, Button, Card, Input, Select, Skeleton, Switch, Timeline, Tooltip } from 'antd'
import { DeleteOutlined, PlusOutlined, SafetyOutlined, SaveOutlined, UndoOutlined } from '@ant-design/icons'
import { api, isBox, type GroupView, type KillSwitchConfig, type KillSwitchEvent, type ProtectedService } from '../../api'

const lines = (text: string) => text.split(/\r?\n/).map((v) => v.trim()).filter(Boolean)

interface DraftService extends Omit<ProtectedService, 'suffixes' | 'cidrs'> {
  key: number
  suffixes: string
  cidrs: string
}

let nextKey = 1
const toDraft = (s: ProtectedService): DraftService => ({
  ...s,
  key: nextKey++,
  suffixes: (s.suffixes ?? []).join('\n'),
  cidrs: (s.cidrs ?? []).join('\n'),
})

// 受保护的服务：桌面客户端和路由盒共用（接口形状相同，路由盒的在 /box/killswitch）。
export default function ProtectedServicesCard({ onSaved }: { onSaved?: () => void }) {
  const { message } = App.useApp()
  const [config, setConfig] = useState<KillSwitchConfig>()
  const [drafts, setDrafts] = useState<DraftService[]>([])
  const [dnsListen, setDnsListen] = useState('')
  const [groups, setGroups] = useState<GroupView[]>([])
  const [dirty, setDirty] = useState(false)
  const [saving, setSaving] = useState(false)

  const reset = useCallback((c: KillSwitchConfig) => {
    setConfig(c)
    setDrafts((c.services ?? []).map(toDraft))
    setDnsListen(c.dns_listen)
    setDirty(false)
  }, [])

  useEffect(() => {
    api.killswitch.config().then(reset, (e: Error) => message.error(e.message))
    api.groups.list().then(setGroups, (e: Error) => message.error(e.message))
  }, [message, reset])

  const patch = (key: number, change: Partial<DraftService>) => {
    setDrafts((list) => list.map((d) => (d.key === key ? { ...d, ...change } : d)))
    setDirty(true)
  }

  const save = async () => {
    setSaving(true)
    try {
      reset(
        await api.killswitch.update({
          dns_listen: dnsListen,
          services: drafts.map((d) => ({
            name: d.name,
            enabled: d.enabled,
            group: d.group,
            suffixes: lines(d.suffixes),
            cidrs: lines(d.cidrs),
          })),
        }),
      )
      message.success(isBox() ? '已保存，规则会自动同步到内核和防火墙' : '已保存，规则会自动同步到内核')
      onSaved?.()
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  const strictGroups = groups.filter((g) => g.strict).map((g) => ({ label: g.name, value: g.name }))

  return (
    <Card
      title={
        <span className="card-title">
          <SafetyOutlined />
          受保护的服务
        </span>
      }
      extra={
        <div className="page-toolbar">
          <Button
            icon={<PlusOutlined />}
            onClick={() => {
              setDrafts((l) => [...l, { key: nextKey++, name: '', enabled: true, group: strictGroups[0]?.value ?? '', suffixes: '', cidrs: '' }])
              setDirty(true)
            }}
          >
            添加服务
          </Button>
          <Button icon={<UndoOutlined />} disabled={!dirty} onClick={() => config && reset(config)}>
            放弃修改
          </Button>
          <Button type="primary" icon={<SaveOutlined />} disabled={!dirty} loading={saving} onClick={save}>
            保存
          </Button>
        </div>
      }
    >
      <div className="hint">
        这里的域名自动走所选的严格代理组，代理故障时会断开；其他流量不受影响。填了固定 IP 网段的服务更可靠：只有域名时，拦不住浏览器 DoH 和硬编码 IP。
      </div>
      {!config && <Skeleton active paragraph={{ rows: 4 }} />}
      <div className="service-list">
        {drafts.map((d) => (
          <div className="service-item" key={d.key}>
            <div className="service-head">
              <Switch aria-label="启用保护" checked={d.enabled} onChange={(enabled) => patch(d.key, { enabled })} />
              <Input aria-label="服务名称" placeholder="服务名称" className="service-name" value={d.name} onChange={(e) => patch(d.key, { name: e.target.value })} />
              <Select aria-label="严格代理组" placeholder="严格代理组" className="service-group" value={d.group || undefined} options={strictGroups} onChange={(group: string) => patch(d.key, { group })} />
              <Tooltip title="删除（保存后生效）">
                <Button
                  danger
                  type="text"
                  className="service-delete"
                  aria-label="删除服务"
                  icon={<DeleteOutlined />}
                  onClick={() => {
                    setDrafts((l) => l.filter((x) => x.key !== d.key))
                    setDirty(true)
                  }}
                />
              </Tooltip>
            </div>
            <div className="service-fields">
              <label>
                <span className="hint">域名后缀（每行一个）</span>
                <Input.TextArea className="mono-input" autoSize={{ minRows: 3, maxRows: 12 }} value={d.suffixes} spellCheck={false} onChange={(e) => patch(d.key, { suffixes: e.target.value })} />
              </label>
              <label>
                <span className="hint">固定 IP 网段（可选，每行一个）</span>
                <Input.TextArea className="mono-input" autoSize={{ minRows: 3, maxRows: 12 }} value={d.cidrs} spellCheck={false} onChange={(e) => patch(d.key, { cidrs: e.target.value })} />
              </label>
            </div>
          </div>
        ))}
      </div>
    </Card>
  )
}

export function EventTimeline({ events, empty = '暂无事件（服务重启后清空）。' }: { events?: KillSwitchEvent[] | null; empty?: string }) {
  if (!events?.length) return <div className="hint">{empty}</div>
  return (
    <Timeline
      items={events.slice(0, 30).map((e) => ({
        color: e.level === 'error' ? 'red' : e.level === 'warn' ? 'orange' : 'green',
        children: (
          <span className="selectable">
            <span className="event-time">{new Date(e.time).toLocaleString()}</span>
            {e.message}
          </span>
        ),
      }))}
    />
  )
}
