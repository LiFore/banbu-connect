import { useEffect, useState } from 'react'
import { Card, Drawer, Empty, Segmented, Space, Table, Tag, Timeline, Tooltip } from 'antd'
import { HistoryOutlined, WifiOutlined } from '@ant-design/icons'
import { serverApi, type AccessMode, type AccessSession, type Device, type DeviceAccess } from '../../serverApi'
import { RowAction, tableEmpty } from '../common'

// 客户端接入状态：客户端此刻怎么上网（经路由盒、受信 Wi-Fi 直连、本机代理、阻断、没有使用代理），
// 状态变化时立即上报、每分钟心跳，与诊断数据分开记录（internal/dist/access.go）。

export const ACCESS_MODES: Record<AccessMode, { color: string; text: string; desc: string }> = {
  box: { color: 'green', text: '经路由盒', desc: '在路由盒网络上，流量交给路由盒出口' },
  trusted_wifi: { color: 'cyan', text: '受信 Wi-Fi 直连', desc: '在受信 Wi-Fi 上直连，没有发现路由盒' },
  local: { color: 'blue', text: '本机代理', desc: '经本机内核（TUN 或系统代理）' },
  hold: { color: 'red', text: '阻断', desc: '受保护的服务被拦截（出口不符合或正在确认）' },
  off: { color: 'default', text: '未使用代理', desc: '内核没有运行，也不在受信网络上' },
}

const time = (v?: string) => (v ? new Date(v).toLocaleString() : '—')

function duration(from: string, to?: string): string {
  const ms = (to ? new Date(to) : new Date()).getTime() - new Date(from).getTime()
  if (!(ms > 0)) return '—'
  const m = Math.floor(ms / 60000)
  if (m < 1) return '不到 1 分钟'
  if (m < 60) return `${m} 分钟`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h} 小时 ${m % 60} 分钟`
  return `${Math.floor(h / 24)} 天 ${h % 24} 小时`
}

export function AccessTag({ mode }: { mode: AccessMode }) {
  const m = ACCESS_MODES[mode] ?? { color: 'default', text: mode, desc: '' }
  return (
    <Tooltip title={m.desc}>
      <Tag color={m.color}>{m.text}</Tag>
    </Tooltip>
  )
}

function netText(a: DeviceAccess): string {
  const n = a.state.network
  if (!n.interface) return '没有网络'
  return `${n.wifi ? 'Wi-Fi' : '有线'}（${n.interface}）${n.trusted ? ' · 受信' : ''}`
}

type Row = { device: Device; access?: DeviceAccess }

export default function DeviceAccessCard({ devices }: { devices: Device[] }) {
  const [list, setList] = useState<DeviceAccess[]>()
  const [historyFor, setHistoryFor] = useState<Device>()

  useEffect(() => {
    const load = () => serverApi.deviceAccess().then(setList, () => setList((l) => l ?? []))
    load()
    const timer = setInterval(load, 30_000)
    return () => clearInterval(timer)
  }, [])

  const byId = new Map((list ?? []).map((a) => [a.device_id, a]))
  const rows: Row[] = devices.filter((d) => !d.revoked).map((d) => ({ device: d, access: byId.get(d.id) }))

  return (
    <Card
      title={
        <span className="card-title">
          <WifiOutlined />
          接入状态
        </span>
      }
      extra={<span className="hint">客户端状态变化时立即上报，每分钟心跳；3 分钟没有上报算离线</span>}
    >
      <Table<Row>
        rowKey={(r) => r.device.id}
        size="small"
        dataSource={rows}
        pagination={false}
        locale={{ emptyText: tableEmpty(list === undefined, '还没有客户端') }}
        columns={[
          { title: '客户端', render: (_, r) => r.device.name },
          {
            title: '接入方式',
            width: 200,
            render: (_, r) =>
              !r.access ? (
                <span className="hint">没有上报（客户端版本较旧或没有运行）</span>
              ) : (
                <Space size={4}>
                  <AccessTag mode={r.access.state.mode} />
                  {!r.access.online && (
                    <Tooltip title={`最后一次上报 ${time(r.access.reported_at)}`}>
                      <Tag>离线</Tag>
                    </Tooltip>
                  )}
                </Space>
              ),
          },
          {
            title: '详情',
            render: (_, r) => {
              const a = r.access
              if (!a) return '—'
              const s = a.state
              return (
                <div>
                  <div>
                    {s.box_id && (
                      <span>
                        路由盒 {s.box_id} <span className="hint">{s.box_addr}</span> ·{' '}
                      </span>
                    )}
                    内核{s.core_running ? `运行中${s.tun ? '（TUN）' : '（系统代理）'}` : '未运行'}
                    {s.gate && ' · 闸门直连'}
                  </div>
                  <div className="hint">
                    {netText(a)}
                    {s.reason && ` · ${s.reason}`}
                  </div>
                </div>
              )
            },
          },
          {
            title: '持续',
            width: 150,
            render: (_, r) =>
              r.access ? (
                <Tooltip title={`从 ${time(r.access.since)} 起`}>
                  {duration(r.access.since, r.access.online ? undefined : r.access.reported_at)}
                </Tooltip>
              ) : (
                '—'
              ),
          },
          { title: '最近上报', width: 180, render: (_, r) => time(r.access?.reported_at) },
          {
            title: '',
            width: 50,
            render: (_, r) => <RowAction title="接入历史" icon={<HistoryOutlined />} onClick={() => setHistoryFor(r.device)} />,
          },
        ]}
      />
      <Drawer
        title={historyFor ? `接入历史：${historyFor.name}` : ''}
        open={!!historyFor}
        size="large"
        onClose={() => setHistoryFor(undefined)}
        destroyOnHidden
      >
        {historyFor && <AccessHistory device={historyFor} />}
      </Drawer>
    </Card>
  )
}

function AccessHistory({ device }: { device: Device }) {
  const [days, setDays] = useState(7)
  const [list, setList] = useState<AccessSession[]>()

  useEffect(() => {
    let alive = true
    serverApi.deviceAccessHistory(device.id, days).then(
      (l) => alive && setList(l),
      () => alive && setList([]),
    )
    return () => {
      alive = false
    }
  }, [device.id, days])

  return (
    <Space orientation="vertical" size={16} style={{ width: '100%' }}>
      <Segmented
        value={days}
        onChange={(v) => {
          setList(undefined)
          setDays(v as number)
        }}
        options={[
          { label: '1 天', value: 1 },
          { label: '7 天', value: 7 },
          { label: '30 天', value: 30 },
          { label: '90 天', value: 90 },
        ]}
      />
      {list === undefined ? null : list.length === 0 ? (
        <Empty description="这段时间没有上报" />
      ) : (
        <Timeline
          items={list.map((s, i) => ({
            key: `${s.start_at}-${i}`,
            color: s.end_at ? 'gray' : 'green',
            content: (
              <div>
                <Space size={4}>
                  <AccessTag mode={s.mode} />
                  {s.box_id && <span>路由盒 {s.box_id}</span>}
                  {!s.end_at && <Tag color="green">进行中</Tag>}
                </Space>
                <div className="hint">
                  {time(s.start_at)} → {s.end_at ? time(s.end_at) : '现在'}（{duration(s.start_at, s.end_at || undefined)}）
                </div>
              </div>
            ),
          }))}
        />
      )}
    </Space>
  )
}
