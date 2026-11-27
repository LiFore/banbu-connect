import { useCallback, useEffect, useState } from 'react'
import { Alert, App, Button, Drawer, Empty, Input, Modal, Select, Space, Table, Tag, Typography } from 'antd'
import { BarChartOutlined, CheckOutlined, CloseOutlined, EditOutlined, MonitorOutlined, PlusOutlined, ReloadOutlined, StopOutlined } from '@ant-design/icons'
import { useNavigate } from 'react-router-dom'
import { serverApi, type Device, type DeviceDiagnostics, type Enrollment, type ServerInfo, type User } from '../../serverApi'
import { PageHeader, RowAction, RowDelete, tableEmpty } from '../common'
import DeviceAccessCard from './DeviceAccess'
import { DIAG_KINDS, DIAG_SCOPE, DiagRecordTable, DiagSnapshotView } from '../diagnostics'

const time = (v: string) => (v ? new Date(v).toLocaleString() : '—')

// 客户端：每台 Banbu Connect 一个令牌，在客户端「高级 → 服务端」里绑定。
export default function Devices() {
  const { message, modal } = App.useApp()
  const navigate = useNavigate()
  const [devices, setDevices] = useState<Device[]>([])
  const [users, setUsers] = useState<User[]>([])
  const [info, setInfo] = useState<ServerInfo>()
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [created, setCreated] = useState<{ device: Device; token: string }>()
  const [diagFor, setDiagFor] = useState<Device>()
  const [enrollments, setEnrollments] = useState<Enrollment[]>([])
  const [approving, setApproving] = useState<Enrollment>()
  const [approveName, setApproveName] = useState('')
  const [renaming, setRenaming] = useState<Device>()
  const [newName, setNewName] = useState('')

  const reload = useCallback(
    () =>
      Promise.all([serverApi.devices(), serverApi.enrollments(), serverApi.users()])
        .then(([d, e, u]) => {
          setDevices(d)
          setUsers(u)
          setEnrollments(e)
        })
        .catch((e: Error) => message.error(e.message))
        .finally(() => setLoading(false)),
    [message],
  )

  useEffect(() => {
    void reload()
    serverApi.info().then(setInfo, () => undefined)
  }, [reload])

  const create = async () => {
    setSaving(true)
    try {
      setCreated(await serverApi.createDevice())
      await reload()
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  const act = (f: () => Promise<unknown>, ok: string) =>
    f()
      .then(() => message.success(ok))
      .catch((e: Error) => message.error(e.message))
      .finally(reload)

  const copy = (text: string) =>
    navigator.clipboard.writeText(text).then(
      () => message.success('已复制'),
      () => message.error('复制失败，请手动选择'),
    )

  return (
    <Space orientation="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader
        title="客户端"
        count={devices.length}
        actions={
          <Button type="primary" icon={<PlusOutlined />} loading={saving} onClick={() => void create()}>
            新建
          </Button>
        }
      />
      {enrollments.length > 0 && (
        <EnrollmentTable
          list={enrollments}
          onApprove={(e) => {
            setApproveName(e.name)
            setApproving(e)
          }}
          onReject={(e) => act(() => serverApi.rejectEnrollment(e.id), '已拒绝')}
          onDelete={(e) => act(() => serverApi.deleteEnrollment(e.id), '已删除')}
        />
      )}
      <Table<Device>
        rowKey="id"
        size="small"
        dataSource={devices}
        pagination={false}
        locale={{ emptyText: tableEmpty(loading, '还没有客户端') }}
        columns={[
          {
            title: '名称',
            dataIndex: 'name',
            render: (v: string, d) => (
              <Space size={4} wrap>
                {v}
                {d.revoked && <Tag>已吊销</Tag>}
                {d.name_custom && d.reported_name && d.reported_name !== v && (
                  <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                    电脑名：{d.reported_name}
                  </Typography.Text>
                )}
              </Space>
            ),
          },
          {
            // 设备是主要对象，所属用户只作辅助：放在名称后面，淡色小号。
            title: '用户',
            dataIndex: 'user_id',
            width: 160,
            render: (v: number, d) => (
              <Select
                size="small"
                variant="borderless"
                allowClear
                disabled={d.revoked}
                value={v || undefined}
                placeholder="未分配"
                style={{ width: 140, color: 'var(--ant-color-text-secondary)' }}
                options={users.map((u) => ({ value: u.id, label: u.name }))}
                onChange={(uid) => void act(() => serverApi.setDeviceUser(d.id, uid ?? 0), '已更新')}
              />
            ),
          },
          { title: '最近联系', dataIndex: 'last_seen', width: 180, render: time },
          { title: 'IP', dataIndex: 'last_ip', width: 140, render: (v: string) => v || '—' },
          { title: '版本', dataIndex: 'app_version', width: 100, render: (v: string) => v || '—' },
          {
            title: '系统',
            dataIndex: 'os_version',
            width: 220,
            render: (v: string, d) => v || d.platform || '—',
          },
          { title: '配置版本', dataIndex: 'profile_version', width: 90, render: (v: number) => (v ? `第 ${v} 版` : '—') },
          { title: '诊断上报', dataIndex: 'diag_at', width: 180, render: time },
          {
            title: '操作',
            width: 180,
            render: (_, d) => (
              <Space size={0}>
                <RowAction
                  title="改名"
                  icon={<EditOutlined />}
                  onClick={() => {
                    setNewName(d.name)
                    setRenaming(d)
                  }}
                />
                <RowAction title="诊断数据" icon={<MonitorOutlined />} onClick={() => setDiagFor(d)} />
                <RowAction title="遥测（代理流量与 Claude Code 用量）" icon={<BarChartOutlined />} onClick={() => navigate(`/telemetry?view=detail&range=24h&client=d${d.id}`)} />
                {!d.revoked && (
                  <RowAction
                    title="吊销"
                    icon={<StopOutlined />}
                    onClick={() =>
                      modal.confirm({
                        title: `吊销「${d.name}」`,
                        content: '吊销后这台客户端不能再同步配置和更新，已下发的配置仍保留在客户端上。',
                        okText: '吊销',
                        okButtonProps: { danger: true },
                        cancelText: '取消',
                        onOk: () => act(() => serverApi.revokeDevice(d.id), '已吊销'),
                      })
                    }
                  />
                )}
                <RowDelete confirm={`删除「${d.name}」？它的令牌会失效。`} onConfirm={() => act(() => serverApi.deleteDevice(d.id), '已删除')} />
              </Space>
            ),
          },
        ]}
      />

      <DeviceAccessCard devices={devices} />

      <Modal
        title={renaming ? `修改「${renaming.name}」的名称` : ''}
        open={!!renaming}
        okText="保存"
        cancelText="取消"
        okButtonProps={{ disabled: !newName.trim() || newName.trim() === renaming?.name }}
        onCancel={() => setRenaming(undefined)}
        onOk={() =>
          renaming &&
          act(() => serverApi.renameDevice(renaming.id, newName.trim()), '已改名').then(() => setRenaming(undefined))
        }
        footer={(_, { OkBtn, CancelBtn }) => (
          <>
            {renaming?.name_custom && (
              <Button
                style={{ float: 'left' }}
                onClick={() =>
                  renaming && act(() => serverApi.renameDevice(renaming.id, ''), '已恢复自动命名').then(() => setRenaming(undefined))
                }
              >
                恢复自动命名
              </Button>
            )}
            <CancelBtn />
            <OkBtn />
          </>
        )}
        destroyOnHidden
      >
        <Space orientation="vertical" style={{ width: '100%' }}>
          <Input
            value={newName}
            maxLength={64}
            showCount
            autoFocus
            placeholder="客户端名称"
            onChange={(e) => setNewName(e.target.value)}
            onPressEnter={() => {
              const name = newName.trim()
              if (renaming && name && name !== renaming.name) {
                void act(() => serverApi.renameDevice(renaming.id, name), '已改名').then(() => setRenaming(undefined))
              }
            }}
          />
          <Typography.Text type="secondary">
            改名后，客户端上报的电脑名不再覆盖这个名称。
            {renaming?.reported_name ? `电脑名：${renaming.reported_name}。` : '这台客户端还没有上报过电脑名。'}
            {renaming?.name_custom && '「恢复自动命名」改回用电脑名。'}
          </Typography.Text>
        </Space>
      </Modal>

      <Drawer title={diagFor ? `诊断数据：${diagFor.name}` : ''} open={!!diagFor} size="large" onClose={() => setDiagFor(undefined)} destroyOnHidden>
        {diagFor && <DiagnosticsView device={diagFor} />}
      </Drawer>

      <Modal
        title="授权接入"
        open={!!approving}
        okText="授权"
        cancelText="取消"
        okButtonProps={{ disabled: !approveName.trim() }}
        onOk={() => {
          if (!approving) return
          const e = approving
          setApproving(undefined)
          void act(() => serverApi.approveEnrollment(e.id, approveName), '已授权，客户端点「刷新授权」即可完成绑定')
        }}
        onCancel={() => setApproving(undefined)}
        destroyOnHidden
      >
        {approving && (
          <Space orientation="vertical" style={{ width: '100%' }}>
            <Typography.Text type="secondary">
              {approving.platform} {approving.os_version}，客户端 {approving.app_version || '—'}，来自 {approving.ip || '—'}
            </Typography.Text>
            <Input placeholder="客户端名称" maxLength={64} value={approveName} onChange={(e) => setApproveName(e.target.value)} />
          </Space>
        )}
      </Modal>

      <Modal title="绑定信息" open={!!created} footer={<Button onClick={() => setCreated(undefined)}>完成</Button>} onCancel={() => setCreated(undefined)}>
        {created && (
          <Space orientation="vertical" style={{ width: '100%' }}>
            <Alert type="warning" showIcon title="令牌只显示这一次，关闭后无法再查看" />
            <Typography.Text>在客户端「高级 → 服务端」里填写。名称和系统信息在客户端第一次连接后自动显示：</Typography.Text>
            <Field label="服务端地址" value={window.location.origin} onCopy={copy} />
            <Field label="客户端令牌" value={created.token} onCopy={copy} />
            {info?.self_signed && <Field label="证书指纹（自签名证书必填）" value={info.fingerprint} onCopy={copy} />}
            {info?.plain_http && <Typography.Text type="secondary">服务端在反向代理后面，客户端按系统证书校验，不用填指纹。</Typography.Text>}
          </Space>
        )}
      </Modal>
    </Space>
  )
}

function Field({ label, value, onCopy }: { label: string; value: string; onCopy: (v: string) => void }) {
  return (
    <div>
      <Typography.Text type="secondary">{label}</Typography.Text>
      <Space.Compact style={{ width: '100%' }}>
        <Input readOnly className="mono-input" value={value} />
        <Button onClick={() => onCopy(value)}>复制</Button>
      </Space.Compact>
    </div>
  )
}

// 一台客户端上报的诊断数据：最近的状态快照和事件（保留 30 天）。
function DiagnosticsView({ device }: { device: Device }) {
  const { message } = App.useApp()
  const [kind, setKind] = useState('')
  const [data, setData] = useState<DeviceDiagnostics>()
  const [loading, setLoading] = useState(true)

  const load = useCallback(() => {
    serverApi
      .diagnostics(device.id, kind)
      .then(setData)
      .catch((e: Error) => message.error(e.message))
      .finally(() => setLoading(false))
  }, [device.id, kind, message])

  useEffect(load, [load])

  return (
    <Space orientation="vertical" size={16} style={{ width: '100%' }}>
      <Typography.Text type="secondary">
        客户端绑定后默认上报，用户可以在客户端「服务端」页面关闭。上报：{DIAG_SCOPE.collected}不上报：{DIAG_SCOPE.excluded}
      </Typography.Text>
      {data?.snapshot ? <DiagSnapshotView snapshot={data.snapshot} /> : !loading && <Empty description="这台客户端还没有上报过诊断数据" />}
      <Space>
        <Select
          style={{ width: 180 }}
          value={kind}
          onChange={(v) => {
            setLoading(true)
            setKind(v)
          }}
          options={[{ value: '', label: '全部类型' }, ...Object.entries(DIAG_KINDS).map(([value, label]) => ({ value, label }))]}
        />
        <Button
          icon={<ReloadOutlined />}
          onClick={() => {
            setLoading(true)
            load()
          }}
        >
          刷新
        </Button>
      </Space>
      <DiagRecordTable records={data?.records ?? []} loading={loading} />
    </Space>
  )
}

const ENROLL_STATUS: Record<Enrollment['status'], { color: string; text: string }> = {
  pending: { color: 'processing', text: '待授权' },
  approved: { color: 'success', text: '已授权，待客户端领取' },
  claimed: { color: 'default', text: '已绑定' },
  rejected: { color: 'error', text: '已拒绝' },
}

// 接入申请：客户端提交设备信息，授权后客户端点「刷新授权」自动绑定。
function EnrollmentTable({
  list,
  onApprove,
  onReject,
  onDelete,
}: {
  list: Enrollment[]
  onApprove: (e: Enrollment) => void
  onReject: (e: Enrollment) => void
  onDelete: (e: Enrollment) => void
}) {
  const pending = list.filter((e) => e.status === 'pending').length
  return (
    <div>
      <Typography.Title level={5} style={{ marginTop: 0 }}>
        接入申请{pending > 0 && <Tag color="processing" style={{ marginLeft: 8 }}>{pending} 条待授权</Tag>}
      </Typography.Title>
      <Table<Enrollment>
        rowKey="id"
        size="small"
        dataSource={list}
        pagination={list.length > 10 ? { pageSize: 10, showSizeChanger: false } : false}
        columns={[
          { title: '设备名称', dataIndex: 'name' },
          { title: '平台', width: 220, render: (_, e) => `${e.platform} ${e.os_version}`.trim() || '—' },
          { title: '版本', dataIndex: 'app_version', width: 90, render: (v: string) => v || '—' },
          { title: 'IP', dataIndex: 'ip', width: 140, render: (v: string) => v || '—' },
          { title: '申请时间', dataIndex: 'updated_at', width: 180, render: time },
          {
            title: '状态',
            dataIndex: 'status',
            width: 170,
            render: (v: Enrollment['status']) => <Tag color={ENROLL_STATUS[v]?.color}>{ENROLL_STATUS[v]?.text ?? v}</Tag>,
          },
          {
            title: '操作',
            width: 120,
            render: (_, e) => (
              <Space size={0}>
                {e.status !== 'approved' && <RowAction title={e.status === 'pending' ? '授权' : '重新授权'} icon={<CheckOutlined />} onClick={() => onApprove(e)} />}
                {e.status === 'pending' && <RowAction title="拒绝" icon={<CloseOutlined />} onClick={() => onReject(e)} />}
                <RowDelete confirm={`删除「${e.name}」的申请？已授权的客户端不受影响。`} onConfirm={() => onDelete(e)} />
              </Space>
            ),
          },
        ]}
      />
    </div>
  )
}
