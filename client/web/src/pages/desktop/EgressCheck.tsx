import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { App, Button, Card, Empty, Table, Tag, Tooltip } from 'antd'
import { AimOutlined, SafetyCertificateOutlined } from '@ant-design/icons'
import { api, type Egress, type GateStatus, type ScanReport, type ServiceRouteTest } from '../../api'
import CountryFlag, { countryName } from './CountryFlag'
import { EgressText, LatencyText, RoutePath } from './RoutePreview'

// 同一个出口 IP 用同一种颜色，一眼看出哪些站点、哪些路走的是同一个出口。
const palette = ['blue', 'green', 'purple', 'cyan', 'gold', 'magenta', 'geekblue', 'lime', 'volcano', 'orange']

function EgressCell({ eg, color }: { eg?: Egress; color?: string }) {
  if (!eg) return <span className="hint">—</span>
  if (!eg.ok) {
    return (
      <Tooltip title={eg.error}>
        <Tag color="red">失败</Tag>
      </Tooltip>
    )
  }
  const where = [countryName(eg.country), eg.colo && `接入 ${eg.colo}`].filter(Boolean).join(' · ')
  return (
    <div className="egress-cell">
      <span className="egress-inline">
        <CountryFlag code={eg.country} />
        <Tag color={color} className="mono-input selectable">
          {eg.ip}
        </Tag>
      </span>
      <span className="hint">
        {where}
        {eg.latency_ms ? ` · ${eg.latency_ms} ms` : ''}
      </span>
    </div>
  )
}

function GateCard({ gate }: { gate: GateStatus }) {
  return (
    <Card
      title={
        <span className="card-title">
          <SafetyCertificateOutlined />
          出口闸门
        </span>
      }
    >
      <div className="hint">
        {gate.active
          ? '受保护服务的每个新连接先确认它自己的出口，符合才经受信网络直连，否则经代理。'
          : '已开启闸门直连，但内核没有运行或配置还没生效：受保护服务全部经代理。'}
      </div>
      {gate.groups.length === 0 ? (
        <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有受保护服务的连接" />
      ) : (
        gate.groups.map((g) => (
          <div className="setting-row" key={g.group}>
            <span>
              {g.group}
              <div className="hint">{g.reason || (g.egress.checked_at ? `出口 ${g.egress.ip}` : '还没有连接')}</div>
            </span>
            <span className="setting-control">
              <Tag color={g.allowed ? 'green' : 'orange'}>{g.allowed ? '直连' : '经代理'}</Tag>
              <span className="hint">
                直连 {g.direct} · 经代理 {g.proxied}
              </span>
            </span>
          </div>
        ))
      )}
    </Card>
  )
}

// 出口检测：参照 ippure 的「IP 出口分布」，分别请求各站点的 Cloudflare trace，
// 看每个站点、每条路看到的出口。分流的网络里，同一时刻不同站点的出口可能不同。
export default function EgressCheck() {
  const { message } = App.useApp()
  const [report, setReport] = useState<ScanReport>()
  const [scanning, setScanning] = useState(false)
  const [gate, setGate] = useState<GateStatus>()
  const [services, setServices] = useState<ServiceRouteTest[]>()

  useEffect(() => {
    const load = () => api.desktop.status().then((s) => setGate(s.network.gate), () => undefined)
    void load()
    const timer = setInterval(load, 3000)
    return () => clearInterval(timer)
  }, [])

  const scan = async () => {
    setScanning(true)
    try {
      // Google、GitHub 没有 Cloudflare trace，按规则实测，和站点检测一起跑。
      const [rep, svc] = await Promise.all([api.network.scan(), api.network.services().catch(() => undefined)])
      setReport(rep)
      setServices(svc)
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setScanning(false)
    }
  }

  const { rows, colors } = useMemo(() => {
    const colors = new Map<string, string>()
    const cells = new Map<string, Egress>()
    for (const r of report?.results ?? []) {
      cells.set(`${r.site}\u0000${r.path}`, r.egress)
      if (r.egress.ok && !colors.has(r.egress.ip)) colors.set(r.egress.ip, palette[colors.size % palette.length])
    }
    const rows = (report?.sites ?? []).map((s) => ({
      key: s.name,
      site: s,
      cells: Object.fromEntries((report?.paths ?? []).map((p) => [p.key, cells.get(`${s.name}\u0000${p.key}`)])),
    }))
    return { rows, colors }
  }, [report])

  const columns = [
    {
      title: '站点',
      key: 'site',
      fixed: 'left' as const,
      render: (_: unknown, r: (typeof rows)[number]) => (
        <span>
          {r.site.name}
          {r.site.protected && (
            <Tag color="blue" style={{ marginLeft: 6 }}>
              受保护
            </Tag>
          )}
        </span>
      ),
    },
    ...(report?.paths ?? []).map((p) => ({
      title: (
        <Tooltip title={p.hint}>
          <span>{p.label}</span>
        </Tooltip>
      ),
      key: p.key,
      render: (_: unknown, r: (typeof rows)[number]) => {
        const eg = r.cells[p.key]
        return <EgressCell eg={eg} color={eg?.ok ? colors.get(eg.ip) : undefined} />
      },
    })),
  ]

  return (
    <div className="desktop-stack">
      {gate?.enabled && <GateCard gate={gate} />}
      <Card
        title={
          <span className="card-title">
            <AimOutlined />
            出口检测
          </span>
        }
        extra={
          <Button type="primary" loading={scanning} onClick={scan}>
            {report ? '重新检测' : '开始检测'}
          </Button>
        }
      >
        <div className="hint">
          分别请求每个站点的 Cloudflare trace，看各条路上它看到的出口 IP、地区和接入点。同一种颜色是同一个出口。路由盒或代理按域名分流时，不同站点的出口可能不同：受保护服务应和预期的出口一致。
        </div>
        {report ? (
          <Table size="small" pagination={false} scroll={{ x: 'max-content' }} columns={columns} dataSource={rows} />
        ) : (
          <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="点「开始检测」，大约需要 10 秒" />
        )}
      </Card>
      {services && (
        <Card
          size="small"
          title={<span className="card-title">Google、GitHub 的出口（按规则实测）</span>}
          extra={<Link to="/advanced/routes">测试其他域名</Link>}
        >
          <div className="hint">
            这两个服务没有回显出口的地址：经本机代理真实请求一次，从内核读出走的规则和线路，再经这条线路测出口。
          </div>
          <Table<ServiceRouteTest>
            size="small"
            rowKey="name"
            pagination={false}
            scroll={{ x: 'max-content' }}
            dataSource={services}
            columns={[
              { title: '服务', dataIndex: 'name', fixed: 'left' },
              { title: '出口', key: 'egress', render: (_, r) => (r.egress ? <EgressText eg={r.egress} /> : <Tooltip title={r.error}><Tag color="red">失败</Tag></Tooltip>) },
              { title: '线路', key: 'path', render: (_, r) => <RoutePath r={r} /> },
              { title: '规则', key: 'rule', render: (_, r) => (r.rule ? <code>{r.rule}{r.rule_payload ? ` ${r.rule_payload}` : ''}</code> : <span className="hint">—</span>) },
              { title: '延迟', key: 'latency', render: (_, r) => <LatencyText r={r} /> },
            ]}
          />
        </Card>
      )}
    </div>
  )
}
