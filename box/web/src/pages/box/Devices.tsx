import { useCallback, useEffect, useState } from 'react'
import { Alert, App, Button, Card, Checkbox, Empty, Form, Input, Modal, Radio, Segmented, Select, Space, Switch, Table, Tag, Tooltip } from 'antd'
import { DeleteOutlined, EditOutlined, PlusOutlined, WarningOutlined } from '@ant-design/icons'
import { api, type BoxClient, type DevicePolicy, type LanDevice, type TimeWindow } from '../../api'
import { formatBytes, PageHeader, RowAction } from '../common'
import RuleListEditor from '../rules/RuleListEditor'

const egressLabel: Record<string, string> = { default: '跟随全局', direct: '全部直连', proxy: '全部代理', group: '指定代理组' }
const dayLabels = ['一', '二', '三', '四', '五', '六', '日']

const defaultPolicy = (ip: string): DevicePolicy => ({ ip, egress: 'default', access: 'allow', schedule: [], rules: [] })

// 局域网设备：按设备设置出口、上网控制、自定义规则和绕过内核。
// 出口、上网控制、自定义规则按 IP（ADR-034/036/041）；绕过内核按 MAC（ADR-040）。
export default function Devices() {
  const { message } = App.useApp()
  const [list, setList] = useState<LanDevice[]>()
  const [groups, setGroups] = useState<string[]>([])
  const [editing, setEditing] = useState<LanDevice>()
  const [filter, setFilter] = useState<'all' | 'clients'>('all')

  const load = useCallback(() => {
    api.box.devices().then(setList, (e: Error) => message.error(e.message))
  }, [message])

  useEffect(() => {
    load()
    api.groups.list().then((g) => setGroups(g.map((x) => x.name)), () => undefined)
    const t = setInterval(load, 5000)
    return () => clearInterval(t)
  }, [load])

  const clients = list?.filter((d) => d.client) ?? []
  const clientsOnline = clients.filter((d) => d.client?.online).length
  const shown = filter === 'clients' ? clients : list

  return (
    <div className="desktop-stack">
      <PageHeader
        title="设备"
        count={list?.length}
        actions={
          <Segmented<'all' | 'clients'>
            size="small"
            value={filter}
            onChange={setFilter}
            options={[
              { value: 'all', label: '全部' },
              { value: 'clients', label: `Banbu Connect ${clientsOnline}/${clients.length}` },
            ]}
          />
        }
      />
      <Alert
        type="info"
        showIcon
        title="设备策略按 IP 生效：请在路由器的 DHCP 里给需要管控的设备绑定静态地址，IP 变了策略不会跟着走。"
      />
      <Card styles={{ body: { padding: 0 } }}>
        <Table<LanDevice>
          rowKey={(d) => d.mac || `ip:${d.ip}`}
          size="small"
          loading={!list}
          dataSource={shown}
          pagination={false}
          locale={{
            emptyText: (
              <Empty
                description={
                  filter === 'clients'
                    ? '还没有 Banbu Connect 客户端来过：客户端绑定同一个管理服务（或填了同一个路由盒口令）后，在这个网络上会自动接入'
                    : '没有发现设备（读取 DHCP 租约和邻居表）'
                }
              />
            ),
          }}
          scroll={{ x: 900 }}
          columns={[
            {
              title: '设备',
              render: (_, d) => (
                <div>
                  <span className={d.online ? 'device-dot online' : 'device-dot'} />
                  {/* 备注名优先，其次客户端自报的电脑名（比 DHCP 主机名好认），主机名不同时附在后面 */}
                  {d.name || d.client?.name || d.hostname || (d.mac ? '未命名设备' : d.client ? 'Banbu Connect 客户端' : '未知设备')}
                  {d.hostname && d.hostname !== (d.name || d.client?.name || d.hostname) && <span className="hint"> {d.hostname}</span>}
                  {d.client && <ClientLine c={d.client} mac={!!d.mac} />}
                  {!d.mac && !d.client && (
                    <Tooltip title="设置了策略，但现在没有设备使用这个地址：设备的 IP 可能变了">
                      <Tag color="warning" style={{ marginLeft: 6 }}>
                        <WarningOutlined /> 无设备
                      </Tag>
                    </Tooltip>
                  )}
                </div>
              ),
            },
            { title: 'IP', dataIndex: 'ip', width: 130, render: (v?: string) => <span className="mono">{v || '—'}</span> },
            {
              title: 'MAC',
              dataIndex: 'mac',
              width: 170,
              render: (v: string | undefined, d) =>
                v ? (
                  <span className="mono">
                    {v}
                    {d.private_mac && (
                      <Tooltip title="私有（随机）MAC：手机可能定期更换，按 MAC 的「绕过内核」会失效">
                        <Tag style={{ marginLeft: 4 }}>私有</Tag>
                      </Tooltip>
                    )}
                  </span>
                ) : (
                  '—'
                ),
            },
            {
              title: '策略',
              render: (_, d) => <PolicyTags d={d} />,
            },
            {
              title: '经内核流量',
              width: 150,
              render: (_, d) =>
                d.bypass ? (
                  <span className="hint">不经过内核</span>
                ) : d.usage ? (
                  <Tooltip title={`当前 ${d.usage.connections} 个连接；面板启动以来累计`}>
                    {formatBytes(d.usage.upload + d.usage.download)}
                  </Tooltip>
                ) : (
                  '—'
                ),
            },
            {
              title: '',
              width: 48,
              render: (_, d) => <RowAction title="设置" icon={<EditOutlined />} disabled={!d.ip && !d.mac} onClick={() => setEditing(d)} />,
            },
          ]}
        />
      </Card>
      {editing && (
        <DeviceForm
          device={editing}
          groups={groups}
          onClose={() => setEditing(undefined)}
          onSaved={() => {
            setEditing(undefined)
            load()
          }}
        />
      )}
    </div>
  )
}

// 设备上的 Banbu Connect 客户端：一行说明（版本、系统、是否正经代理入口上网），悬停看最近一次复查的时间。
// 没有 MAC 的是跨网段经代理入口接入的（ADR-084），租约和邻居表里没有它。
function ClientLine({ c, mac }: { c: BoxClient; mac: boolean }) {
  const seen = new Date(c.last_seen)
  const known = !Number.isNaN(seen.getTime()) && seen.getFullYear() > 2000
  const parts = [c.version && `v${c.version}`, c.os, c.entry > 0 ? `经代理入口 ${c.entry} 个连接` : c.online ? '已接入，未经代理入口' : '离线'].filter(Boolean)
  const tip = [
    c.name && `电脑名：${c.name}`,
    known && `最近一次复查路由盒：${seen.toLocaleString()}`,
    !c.version && '旧版客户端，不报名字和版本',
    !mac && '跨网段接入：路由盒的租约和邻居表里没有这台设备',
  ]
    .filter(Boolean)
    .join('\n')
  return (
    <Tooltip title={<span style={{ whiteSpace: 'pre-line' }}>{tip || undefined}</span>}>
      <div className="device-client">
        <Tag color={c.online ? 'green' : undefined}>Banbu Connect</Tag>
        <span className="hint">{parts.join(' · ')}</span>
      </div>
    </Tooltip>
  )
}

function PolicyTags({ d }: { d: LanDevice }) {
  const p = d.policy
  const tags: React.ReactNode[] = []
  if (d.bypass) tags.push(<Tag key="b" color="purple">绕过内核</Tag>)
  if (p && p.egress !== 'default')
    tags.push(
      <Tag key="e" color={p.egress === 'direct' ? 'orange' : 'blue'}>
        {p.egress === 'group' ? p.egress_group : egressLabel[p.egress]}
      </Tag>,
    )
  if (p?.access === 'block') tags.push(<Tag key="a" color="red">断网</Tag>)
  if (p?.access === 'schedule') tags.push(<Tag key="a" color="gold">按时段放行</Tag>)
  if (p && p.rules.length > 0) tags.push(<Tag key="r">{p.rules.length} 条规则</Tag>)
  return tags.length ? <>{tags}</> : <span className="hint">跟随全局</span>
}

function DeviceForm({ device, groups, onClose, onSaved }: { device: LanDevice; groups: string[]; onClose: () => void; onSaved: () => void }) {
  const { message } = App.useApp()
  const [name, setName] = useState(device.name ?? '')
  const [bypass, setBypass] = useState(device.bypass)
  const [p, setP] = useState<DevicePolicy>(device.policy ?? defaultPolicy(device.ip ?? ''))
  const [saving, setSaving] = useState(false)

  const save = async () => {
    setSaving(true)
    try {
      if (device.mac) await api.box.updateDevice(device.mac, { name, bypass })
      if (device.ip) await api.box.updatePolicy(p)
      message.success('已保存，配置稍后重新生效')
      onSaved()
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  const blocksProtected = bypass || p.egress === 'direct'
  const setWindow = (i: number, w: Partial<TimeWindow>) => setP({ ...p, schedule: p.schedule.map((x, j) => (j === i ? { ...x, ...w } : x)) })

  return (
    <Modal
      open
      width={640}
      title={`设置 ${device.name || device.hostname || device.ip || device.mac}`}
      onCancel={onClose}
      onOk={save}
      okText="保存"
      confirmLoading={saving}
      destroyOnHidden
    >
      <Form layout="vertical">
        {device.mac && (
          <>
            <Form.Item label="备注名">
              <Input value={name} maxLength={64} onChange={(e) => setName(e.target.value)} placeholder={device.hostname} />
            </Form.Item>
            <Form.Item
              label="完全绕过内核"
              extra={
                device.private_mac
                  ? '这台设备用的是私有 MAC，可能会更换，更换后不再绕过。'
                  : '按 MAC 生效：适合游戏机、IoT、NAS，流量不经过内核，延迟更低。'
              }
            >
              <Switch checked={bypass} onChange={setBypass} />
            </Form.Item>
          </>
        )}
        {device.ip ? (
          <>
            <Form.Item label="出口" extra="按 IP 生效，排在所有规则之前（含受保护服务）。">
              <Space wrap>
                <Radio.Group value={p.egress} onChange={(e) => setP({ ...p, egress: e.target.value })} disabled={bypass}>
                  {Object.entries(egressLabel).map(([k, v]) => (
                    <Radio.Button key={k} value={k}>
                      {v}
                    </Radio.Button>
                  ))}
                </Radio.Group>
                {p.egress === 'group' && (
                  <Select
                    style={{ width: 180 }}
                    value={p.egress_group}
                    onChange={(g) => setP({ ...p, egress_group: g })}
                    options={groups.map((g) => ({ value: g, label: g }))}
                    placeholder="选择代理组"
                  />
                )}
              </Space>
            </Form.Item>
            {blocksProtected && (
              <Alert
                type="warning"
                showIcon
                style={{ marginBottom: 16 }}
                title="这台设备上，受保护的服务（如 Claude、ChatGPT）将不可用"
                description="受保护服务不允许直连，设备的流量又不经过严格代理组，访问会被阻断（ADR-035）。"
              />
            )}
            <Form.Item label="上网控制" extra="去外网的流量被丢弃；访问路由器和局域网内其他设备不受影响。">
              <Radio.Group
                value={p.access}
                onChange={(e) =>
                  setP({ ...p, access: e.target.value, schedule: e.target.value === 'schedule' && p.schedule.length === 0 ? [{ start: '08:00', end: '21:00' }] : p.schedule })
                }
              >
                <Radio.Button value="allow">不限制</Radio.Button>
                <Radio.Button value="block">断网</Radio.Button>
                <Radio.Button value="schedule">按时段放行</Radio.Button>
              </Radio.Group>
            </Form.Item>
            {p.access === 'schedule' && (
              <Form.Item label="放行时段" extra="不在任何时段内时断网；结束早于开始表示跨午夜。">
                <div className="schedule-list">
                  {p.schedule.map((w, i) => (
                    <Space key={i} wrap className="schedule-row">
                      <Checkbox.Group
                        value={w.days ?? []}
                        onChange={(v) => setWindow(i, { days: v as number[] })}
                        options={dayLabels.map((l, j) => ({ value: j + 1, label: l }))}
                      />
                      <Input type="time" size="small" style={{ width: 110 }} aria-label="开始" value={w.start} onChange={(e) => setWindow(i, { start: e.target.value })} />
                      <span>至</span>
                      <Input type="time" size="small" style={{ width: 110 }} aria-label="结束" value={w.end} onChange={(e) => setWindow(i, { end: e.target.value })} />
                      <RowAction title="删除时段" icon={<DeleteOutlined />} onClick={() => setP({ ...p, schedule: p.schedule.filter((_, j) => j !== i) })} />
                    </Space>
                  ))}
                  <Button size="small" icon={<PlusOutlined />} onClick={() => setP({ ...p, schedule: [...p.schedule, { start: '08:00', end: '21:00' }] })}>
                    添加时段
                  </Button>
                  <span className="hint"> 不勾星期表示每天。</span>
                </div>
              </Form.Item>
            )}
            <Form.Item label="自定义规则" extra="只对这台设备生效，排在设备出口之前；没命中的继续按出口和全局规则走。">
              <RuleListEditor value={p.rules} onChange={(rules) => setP({ ...p, rules })} targets={['DIRECT', 'REJECT', ...groups]} />
            </Form.Item>
          </>
        ) : (
          <Alert type="info" showIcon title="这台设备没有 IP 地址，只能设置备注名和绕过内核。" />
        )}
      </Form>
    </Modal>
  )
}
