import { useEffect, useState } from 'react'
import { App, Button, Card, Descriptions, Empty, Skeleton, Table, Tag, Tooltip } from 'antd'
import { CheckCircleOutlined, CloseCircleOutlined, RadarChartOutlined, ReloadOutlined } from '@ant-design/icons'
import { api, type BoxStatus, type NetworkStatus } from '../../api'
import { PageHeader } from '../common'

// 路由盒自动发现（ADR-073）的详情：经物理网卡的 DNS 查发现用的域名，连解析到的地址做口令挑战应答，
// 通过即视为在路由盒网络上。排查「为什么没发现路由盒」用。

const zero = (t?: string) => !t || t.startsWith('0001')
const fmt = (t?: string) => (zero(t) ? '—' : new Date(t!).toLocaleString())

function summary(b: BoxStatus): { color: string; text: string } {
  if (!b.enabled) return { color: 'default', text: '未开启' }
  if (b.id) return { color: 'green', text: `已发现 ${b.id}` }
  if (b.busy && zero(b.checked_at)) return { color: 'processing', text: '正在发现' }
  if (zero(b.checked_at)) return { color: 'default', text: '还没有发现过' }
  if (b.error) return { color: 'orange', text: '验证失败' }
  return { color: 'default', text: '未发现' }
}

export default function BoxDiscovery() {
  const { message } = App.useApp()
  const [net, setNet] = useState<NetworkStatus>()
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    const load = () => api.desktop.status().then((s) => setNet(s.network), () => undefined)
    load()
    const timer = setInterval(load, 3000)
    return () => clearInterval(timer)
  }, [])

  if (!net) {
    return (
      <Card>
        <Skeleton active paragraph={{ rows: 6 }} />
      </Card>
    )
  }
  const b = net.box
  const s = b ? summary(b) : { color: 'default', text: '不支持' }
  const lookups = b?.trace?.lookups ?? []
  const attempts = b?.trace?.attempts ?? []

  const recheck = async () => {
    setBusy(true)
    try {
      setNet(await api.network.boxRecheck())
      message.success('已开始重新发现，几秒后刷新结果')
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="desktop-stack">
      <PageHeader
        title="路由盒发现"
        actions={
          <Button icon={<ReloadOutlined />} loading={busy || b?.busy} disabled={!b?.enabled} onClick={recheck}>
            立即重新发现
          </Button>
        }
      />
      <Card
        title={
          <span className="card-title">
            <RadarChartOutlined />
            状态
          </span>
        }
        extra={<Tag color={s.color}>{s.text}</Tag>}
      >
        {!b?.enabled ? (
          <p className="hint">
            没有路由盒口令：绑定管理服务后由管理服务下发（「线路与规则」里的路由盒口令）。没有口令时不做发现，只按受信 Wi-Fi
            名单判断受信网络。
          </p>
        ) : (
          <Descriptions column={1} size="small" colon={false} styles={{ label: { width: 140 } }}>
            <Descriptions.Item label="路由盒">
              {b.id ? (
                <span>
                  {b.id} <span className="hint mono-input selectable">{b.addr}</span>
                </span>
              ) : (
                <span className="hint">没有</span>
              )}
            </Descriptions.Item>
            {b.error && (
              <Descriptions.Item label="失败原因">
                <span className="selectable">{b.error}</span>
              </Descriptions.Item>
            )}
            <Descriptions.Item label="查询的域名">
              <code className="selectable">{b.name}</code>，应答端口 <code>{b.port}</code>
            </Descriptions.Item>
            <Descriptions.Item label="网卡">{b.interface || net.network.interface || '—'}</Descriptions.Item>
            <Descriptions.Item label="DNS 服务器">
              {b.dns?.length ? (
                <span className="mono-input selectable">{b.dns.join('、')}</span>
              ) : (
                <span className="hint">{zero(b.checked_at) ? '—' : '没有读到网卡的 DNS 服务器'}</span>
              )}
            </Descriptions.Item>
            <Descriptions.Item label="上次发现">{fmt(b.checked_at)}</Descriptions.Item>
            <Descriptions.Item label="下次发现">{b.busy ? '正在发现…' : fmt(b.next_at)}</Descriptions.Item>
            <Descriptions.Item label="连续失败">
              {b.fails} 次
              <span className="hint">（已发现的路由盒连续失败 {b.max_fails} 次才认为不在了）</span>
            </Descriptions.Item>
          </Descriptions>
        )}
      </Card>
      {b?.enabled && !zero(b.checked_at) && (
        <>
          <Card title="DNS 查询">
            {lookups.length === 0 ? (
              <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="没有可问的 DNS 服务器" />
            ) : (
              <Table
                size="small"
                pagination={false}
                rowKey={(l) => `${l.name}@${l.server}`}
                dataSource={lookups}
                columns={[
                  { title: '域名', dataIndex: 'name', render: (v: string) => <span className="mono-input">{v}</span> },
                  { title: 'DNS 服务器', dataIndex: 'server', render: (v: string) => <span className="mono-input">{v}</span> },
                  {
                    title: '结果',
                    render: (_, l) =>
                      l.error ? (
                        <Tooltip title={l.error}>
                          <Tag color="orange">查不到</Tag>
                        </Tooltip>
                      ) : (
                        <span>
                          {(l.addrs ?? []).map((a) => (
                            <Tag key={a} className="mono-input">
                              {a}
                            </Tag>
                          ))}
                          {(l.ignored ?? []).length > 0 && (
                            <Tooltip title="路由盒总在局域网里，指向公网地址的应答不采用">
                              <Tag color="default">忽略公网地址 {l.ignored!.join('、')}</Tag>
                            </Tooltip>
                          )}
                          {!(l.addrs ?? []).length && !(l.ignored ?? []).length && <span className="hint">没有地址</span>}
                        </span>
                      ),
                  },
                ]}
              />
            )}
            <p className="hint">
              依次问网卡的 DNS 服务器，用第一个给出局域网地址的结果。新名字查不到时再查一次旧版本路由盒的名字。都查不到说明这个网络上没有路由盒。
            </p>
          </Card>
          <Card title="挑战应答">
            {attempts.length === 0 ? (
              <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="没有解析到地址，没有做验证" />
            ) : (
              <Table
                size="small"
                pagination={false}
                rowKey="addr"
                dataSource={attempts}
                columns={[
                  { title: '地址', dataIndex: 'addr', render: (v: string) => <span className="mono-input">{v}</span> },
                  {
                    title: '结果',
                    render: (_, a) =>
                      a.ok ? (
                        <Tag icon={<CheckCircleOutlined />} color="green">
                          验证通过
                        </Tag>
                      ) : (
                        <span>
                          <Tag icon={<CloseCircleOutlined />} color="red">
                            失败
                          </Tag>
                          <span className="hint selectable">{a.error}</span>
                        </span>
                      ),
                  },
                ]}
              />
            )}
            <p className="hint">
              连路由盒的应答端口，用管理服务下发的口令做 HMAC 挑战。连不上一般是路由盒没开发现或防火墙拦了；口令验证不通过说明路由盒和这台客户端的口令不一致。
            </p>
          </Card>
        </>
      )}
    </div>
  )
}
