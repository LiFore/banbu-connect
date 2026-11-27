import { Descriptions, Table, Tag } from 'antd'
import type { DiagEgress, DiagRecord, DiagSnapshot } from '../api'

// 诊断数据的展示，客户端「服务端」页和管理服务「客户端」页共用。

export const DIAG_KINDS: Record<string, string> = {
  egress: '受信 Wi-Fi 出口',
  strict_egress: '严格线路出口',
  network: '网络',
  killswitch: '保护服务',
  health: '内核健康',
  core: '代理内核',
  update: '客户端更新',
}

const LEVEL_COLOR: Record<string, string> = {
  info: 'default',
  warn: 'warning',
  error: 'error',
}
const LEVEL_TEXT: Record<string, string> = {
  info: '信息',
  warn: '警告',
  error: '错误',
}

const STATE_TEXT: Record<string, string> = {
  off: '未启用',
  protecting: '保护中',
  proxy_only: 'TUN 未接管',
  blocking: '内核不可用，已断开',
  error: '规则出错',
  direct: '受信网络直连',
  held: '已阻断',
}

const time = (v?: string) => (v ? new Date(v).toLocaleString() : '—')

function egressText(e?: DiagEgress) {
  if (!e || !e.checked_at || e.checked_at.startsWith('0001')) return '—'
  const latency = e.latency_ms ? `，${e.latency_ms} ms` : ''
  if (!e.ok) return `失败：${e.error ?? ''}${latency}`
  return `${e.ip}${e.country ? `（${e.country}）` : ''}${latency}`
}

export function DiagSnapshotView({ snapshot }: { snapshot: DiagSnapshot }) {
  const n = snapshot.network
  const network = !n.interface
    ? '无网络'
    : `${n.interface}${n.wifi ? `，Wi-Fi${n.trusted ? '（受信）' : ''}${n.ssid_readable ? '' : '，读不到名称'}` : ''}${n.hint ? `：${n.hint}` : ''}`
  return (
    <Descriptions
      column={1}
      size="small"
      bordered
      items={[
        { label: '快照时间', children: time(snapshot.time) },
        {
          label: '保护服务',
          children: `${STATE_TEXT[snapshot.killswitch] ?? snapshot.killswitch}${snapshot.reason ? `：${snapshot.reason}` : ''}`,
        },
        {
          label: '内核',
          children: `${snapshot.core_running ? '运行中' : '未运行'}，DNS ${snapshot.dns_ok ? '正常' : '不可用'}，TUN ${snapshot.tun_ok ? '已接管' : '未接管'}`,
        },
        ...(snapshot.core_error ? [{ label: '内核错误', children: snapshot.core_error }] : []),
        ...(snapshot.tun_error ? [{ label: 'TUN 错误', children: snapshot.tun_error }] : []),
        { label: '网络', children: network },
        { label: '受信 Wi-Fi 出口', children: egressText(snapshot.egress) },
        ...(snapshot.strict ?? []).map((g) => ({
          label: `严格线路「${g.group}」`,
          children: `${egressText(g.egress)}${g.allowed ? '' : '（不可用）'}`,
        })),
      ]}
    />
  )
}

export function DiagRecordTable({ records, loading }: { records: DiagRecord[]; loading?: boolean }) {
  return (
    <Table<DiagRecord>
      rowKey="seq"
      size="small"
      loading={loading}
      dataSource={records}
      pagination={records.length > 50 ? { pageSize: 50, showSizeChanger: false } : false}
      locale={{ emptyText: '没有事件' }}
      expandable={{
        rowExpandable: (r) => !!r.fields && Object.keys(r.fields).length > 0,
        expandedRowRender: (r) => (
          <Descriptions
            column={2}
            size="small"
            items={Object.entries(r.fields ?? {}).map(([k, v]) => ({
              label: k,
              children: <span className="selectable">{v}</span>,
            }))}
          />
        ),
      }}
      columns={[
        {
          title: '时间',
          dataIndex: 'time',
          width: 170,
          render: (v: string) => time(v),
        },
        {
          title: '类型',
          dataIndex: 'kind',
          width: 120,
          render: (v: string) => DIAG_KINDS[v] ?? v,
        },
        {
          title: '级别',
          dataIndex: 'level',
          width: 70,
          render: (v: string) => <Tag color={LEVEL_COLOR[v]}>{LEVEL_TEXT[v] ?? v}</Tag>,
        },
        {
          title: '内容',
          dataIndex: 'message',
          render: (v: string) => <span className="selectable">{v}</span>,
        },
      ]}
    />
  )
}

// 给用户看的收集范围说明。
export const DIAG_SCOPE = {
  collected:
    '出口探测结果（IP、地区、耗时、错误）、保护服务状态变化、代理内核和 TUN 的故障、网络类型变化（网卡、是否 Wi-Fi、是否受信），以及上报时的状态快照。',
  excluded: '访问的网站和连接、流量、Wi-Fi 名称、开关机和休眠、应用启动退出，以及开关代理、切换模式和线路等操作。',
}
