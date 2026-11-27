import { useCallback, useEffect, useState } from 'react'
import { Alert, App, Button, Card, Descriptions, Modal, Skeleton, Space, Switch, Table } from 'antd'
import { BarChartOutlined, CloudServerOutlined, CloudUploadOutlined, DisconnectOutlined, MonitorOutlined, SyncOutlined } from '@ant-design/icons'
import { api, isBox, type ConnBatch, type ConnMinute, type ConnStatsStatus, type DiagStatus, type ServerStatus } from '../../api'
import { formatBytes } from '../common'
import EnrollPanel from './Enroll'
import { DIAG_SCOPE, DiagRecordTable, DiagSnapshotView } from '../diagnostics'

const time = (t?: string) => (t ? new Date(t).toLocaleString() : '—')

// 绑定管理服务：节点、代理组、线路和规则以服务端下发的为准，本地在「高级」里追加；
// 应用更新也从管理服务获取。
export default function ServerBinding() {
  const { message, modal } = App.useApp()
  const [status, setStatus] = useState<ServerStatus>()
  const [busy, setBusy] = useState(false)

  const load = useCallback(() => {
    api.server.status().then(setStatus, (e: Error) => message.error(e.message))
  }, [message])

  useEffect(() => {
    load()
    const timer = setInterval(load, 10000)
    return () => clearInterval(timer)
  }, [load])

  const run = async (action: () => Promise<ServerStatus>, done: string) => {
    setBusy(true)
    try {
      setStatus(await action())
      message.success(done)
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  if (!status) {
    return (
      <Card>
        <Skeleton active paragraph={{ rows: 4 }} />
      </Card>
    )
  }

  if (!status.bound) {
    return (
      <Card
        title={
          <span className="card-title">
            <CloudServerOutlined />
            绑定管理服务
          </span>
        }
      >
        <p className="hint">
          绑定后，节点、代理组、线路和规则以管理服务下发的为准，每 5 分钟同步一次；这里「高级」里的本地配置继续生效，和下发的合并，同名时以下发的为准。
          {isBox() ? '路由盒的程序更新通过 iStore 安装新版本。' : '应用更新也从管理服务获取。'}
        </p>
        <EnrollPanel onBound={load} />
      </Card>
    )
  }

  return (
    <div className="desktop-stack">
      {status.last_error && <Alert type="warning" showIcon title="最近一次同步失败，继续使用上一次的配置" description={status.last_error} />}
      <Card
        title={
          <span className="card-title">
            <CloudServerOutlined />
            管理服务
          </span>
        }
        extra={
          <div className="page-toolbar">
            <Button icon={<SyncOutlined />} loading={busy} onClick={() => run(api.server.sync, '已同步')}>
              立即同步
            </Button>
            <Button
              danger
              icon={<DisconnectOutlined />}
              disabled={busy}
              onClick={() =>
                modal.confirm({
                  title: '解除绑定？',
                  content: isBox()
                    ? '下发的节点、代理组、线路和规则会被移除，只保留本地配置。'
                    : '下发的节点、代理组、线路和规则会被移除，只保留本地配置；之后也不再收到应用更新。',
                  okText: '解除绑定',
                  okButtonProps: { danger: true },
                  cancelText: '取消',
                  onOk: () => run(api.server.unbind, '已解除绑定'),
                })
              }
            >
              解除绑定
            </Button>
          </div>
        }
      >
        <Descriptions
          column={2}
          size="small"
          items={[
            { label: '地址', children: <span className="selectable">{status.url}</span> },
            { label: '证书', children: status.pinned ? '按指纹校验' : '系统证书' },
            { label: '配置版本', children: status.version ? `第 ${status.version} 版` : '—' },
            { label: '发布时间', children: time(status.published_at) },
            { label: '说明', children: status.note || '—' },
            { label: '内容', children: `${status.nodes} 个节点、${status.groups} 个代理组、${status.rules} 条规则` },
            { label: '最近同步', children: time(status.last_sync) },
          ]}
        />
      </Card>
      {/* 诊断上报目前只有桌面客户端有（ADR-028）。 */}
      {!isBox() && <DiagnosticsCard />}
      <ConnStatsCard />
    </div>
  )
}

// 诊断上报：让用户看到上报什么、不上报什么，可以关闭。
function DiagnosticsCard() {
  const { message } = App.useApp()
  const [diag, setDiag] = useState<DiagStatus>()
  const [busy, setBusy] = useState(false)
  const [viewing, setViewing] = useState(false)

  const load = useCallback(() => {
    api.server.diagnostics().then(setDiag, () => undefined)
  }, [])

  useEffect(() => {
    load()
    const timer = setInterval(load, 10000)
    return () => clearInterval(timer)
  }, [load])

  const run = async (action: () => Promise<DiagStatus>, done?: string) => {
    setBusy(true)
    try {
      setDiag(await action())
      if (done) message.success(done)
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  if (!diag) return null
  return (
    <Card
      title={
        <span className="card-title">
          <MonitorOutlined />
          诊断数据上报
        </span>
      }
      extra={
        <div className="page-toolbar">
          <Button disabled={!diag.enabled} onClick={() => setViewing(true)}>
            查看上报内容
          </Button>
          <Button icon={<CloudUploadOutlined />} disabled={!diag.enabled} loading={busy} onClick={() => run(api.server.uploadDiagnostics, '已上报')}>
            立即上报
          </Button>
          <Switch checked={diag.enabled} loading={busy} onChange={(on) => run(() => api.server.setDiagnostics(on))} />
        </div>
      }
    >
      <p className="hint">把出口探测结果和故障上报给管理服务，方便管理员排查连接问题。每 5 分钟一次，出现警告或错误时提前。关闭后不再收集，本机还没上报的也会清掉。</p>
      <p className="hint">上报：{DIAG_SCOPE.collected}</p>
      <p className="hint">不上报：{DIAG_SCOPE.excluded}</p>
      {diag.enabled && (
        <Descriptions
          column={2}
          size="small"
          items={[
            { label: '待上报', children: `${diag.pending} 条事件` },
            { label: '最近上报', children: time(diag.last_upload) },
            ...(diag.last_error ? [{ label: '上报失败', children: diag.last_error }] : []),
          ]}
        />
      )}
      <Modal title="上报内容" open={viewing} width={860} footer={null} onCancel={() => setViewing(false)} destroyOnHidden>
        <p className="hint">每次上报都带下面的状态快照；事件上报成功后从本机删除，这里只列出还没上报的。</p>
        <DiagSnapshotView snapshot={diag.snapshot} />
        <div style={{ height: 16 }} />
        <DiagRecordTable records={diag.records} />
      </Modal>
    </Card>
  )
}

// 代理连接统计上报（ADR-048，BXC-F0-029）：说明上报什么、不上报什么，可以关闭，并显示待上报的内容。
function ConnStatsCard() {
  const { message } = App.useApp()
  const [st, setSt] = useState<ConnStatsStatus>()
  const [busy, setBusy] = useState(false)
  const [viewing, setViewing] = useState(false)

  const load = useCallback(() => {
    api.server.connStats().then(setSt, () => undefined)
  }, [])

  useEffect(() => {
    load()
    const timer = setInterval(load, 15000)
    return () => clearInterval(timer)
  }, [load])

  const run = async (action: () => Promise<ConnStatsStatus>, done?: string) => {
    setBusy(true)
    try {
      setSt(await action())
      if (done) message.success(done)
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  if (!st) return null
  const preview = st.preview
  return (
    <Card
      title={
        <span className="card-title">
          <BarChartOutlined />
          代理连接统计上报
        </span>
      }
      extra={
        <div className="page-toolbar">
          <Button disabled={!st.enabled || !preview} onClick={() => setViewing(true)}>
            查看待上报内容
          </Button>
          <Button
            icon={<CloudUploadOutlined />}
            disabled={!st.enabled || !st.bound}
            loading={busy}
            onClick={() => run(api.server.uploadConnStats, '已上报')}
          >
            立即上报
          </Button>
          <Switch checked={st.enabled} loading={busy} onChange={(on) => run(() => api.server.setConnStats(on))} />
        </div>
      }
    >
      <p className="hint">
        统计经代理组出去的连接，每分钟汇总后上报给管理服务，在管理服务的「遥测」页查看。关闭后不再统计，本机还没上报的也会清掉。
      </p>
      <p className="hint">
        上报：每分钟按代理组和链路节点、访问的域名（没有域名时为目标 IP）、命中的规则{isBox() ? '、局域网设备（IP、MAC、名称）' : '、发起连接的程序、本机的局域网 IP 和 MAC'}
        汇总的流量和连接数；已关闭连接的记录（时间、域名、目标地址、规则、链路、流量）；内核总流量中归不到具体连接的部分。
      </p>
      <p className="hint">不上报：直连（DIRECT）和被拦截（REJECT）的连接、访问的完整网址和内容。</p>
      {st.enabled && (
        <Descriptions
          column={2}
          size="small"
          items={[
            { label: '待上报', children: `${st.pending} 分钟的数据` },
            { label: '最近上报', children: time(st.last_upload) },
            ...(!st.bound ? [{ label: '状态', children: '还没有绑定管理服务，数据先留在本机' }] : []),
            ...(st.last_error ? [{ label: '上报失败', children: st.last_error }] : []),
          ]}
        />
      )}
      <Modal title="待上报内容（最近一分钟）" open={viewing} width={900} footer={null} onCancel={() => setViewing(false)} destroyOnHidden>
        {preview && <ConnPreview batch={preview} />}
      </Modal>
    </Card>
  )
}

function ConnPreview({ batch }: { batch: ConnBatch }) {
  return (
    <Space orientation="vertical" size={12} style={{ width: '100%' }}>
      <Table<ConnMinute>
        size="small"
        rowKey={(m) => `${m.path.join('>')}|${m.host}|${m.rule}|${m.process ?? ''}|${m.source ?? ''}`}
        dataSource={batch.minutes}
        pagination={false}
        scroll={{ x: 760, y: 280 }}
        columns={[
          { title: '域名', dataIndex: 'host', ellipsis: true },
          { title: '链路', dataIndex: 'path', render: (p: string[]) => p.join(' → '), ellipsis: true },
          { title: '规则', key: 'rule', render: (_, m) => (m.payload ? `${m.rule},${m.payload}` : m.rule), ellipsis: true },
          { title: isBox() ? '来源' : '程序', key: 'who', width: 120, render: (_, m) => m.source || m.process || '—' },
          { title: '流量', key: 'bytes', width: 100, render: (_, m) => formatBytes(m.upload + m.download) },
          { title: '新连接', dataIndex: 'conns', width: 70 },
        ]}
      />
      <p className="hint">
        这一分钟关闭的连接 {batch.records.length} 条
        {batch.unattributed?.length ? `；未归属流量 ${formatBytes(batch.unattributed[0].upload + batch.unattributed[0].download)}` : ''}
        {batch.lan?.length ? `；本机局域网地址 ${batch.lan.map((a) => `${a.ip}（${a.mac}）`).join('、')}` : ''}
      </p>
    </Space>
  )
}
