import { type ReactNode, useCallback, useEffect, useState } from 'react'
import { Alert, App, Button, Card, Empty, Form, Input, Modal, Popconfirm, Select, Skeleton, Space, Switch, Table, Tag, Timeline, Typography } from 'antd'
import { ExperimentOutlined, RiseOutlined, SendOutlined, UploadOutlined } from '@ant-design/icons'
import { serverApi, type Device, type DeviceUpdate, type Release, type UpdateMode } from '../../serverApi'
import { PageHeader, RowAction, RowDelete } from '../common'

const modeLabels: Record<UpdateMode, string> = { normal: '普通', push: '主动推送', silent: '静默更新', force: '强制更新' }

const phaseLabels: Record<string, { label: string; color?: string }> = {
  requested: { label: '等待开始' },
  checking: { label: '检查中' },
  downloading: { label: '下载中', color: 'blue' },
  installing: { label: '安装中', color: 'blue' },
  installed: { label: '已安装，待重启', color: 'green' },
  failed: { label: '失败', color: 'red' },
  none: { label: '没有可用更新' },
}

const size = (n: number) => (n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(1)} MB` : `${Math.ceil(n / 1024)} KB`)

interface UploadForm {
  version: string
  target: string
  notes: string
}

// 客户端更新：上传 scripts/build-app.sh 生成的更新包和签名，打开「发布」后客户端检查更新时会收到。
// 灰度：给更新包指定客户端后只有它们能收到，其他客户端仍然拿到不在灰度中的最高版本；清空名单即全量。
// 列表分两列按版本时间线显示：左列是灰度中和还没发布的（含构建脚本自动上传的），右列是已全量发布的正式版本。
export default function Releases() {
  const { message } = App.useApp()
  const [form] = Form.useForm<UploadForm>()
  const [releases, setReleases] = useState<Release[]>([])
  const [targets, setTargets] = useState<string[]>([])
  const [loading, setLoading] = useState(true)
  const [file, setFile] = useState<File>()
  const [sig, setSig] = useState<File>()
  const [uploading, setUploading] = useState(false)
  const [devices, setDevices] = useState<Device[]>([])
  const [canary, setCanary] = useState<{ release: Release; devices: number[] }>()
  const [savingCanary, setSavingCanary] = useState(false)
  const [policy, setPolicy] = useState<{ release: Release; mode: UpdateMode; min: string }>()
  const [savingPolicy, setSavingPolicy] = useState(false)
  const [updates, setUpdates] = useState<DeviceUpdate[]>([])

  const reload = useCallback(
    () =>
      serverApi
        .releases()
        .then((r) => {
          setReleases(r.releases)
          setTargets(r.targets)
        })
        .catch((e: Error) => message.error(e.message))
        .finally(() => setLoading(false)),
    [message],
  )

  useEffect(() => {
    void reload()
    serverApi.devices().then(setDevices, () => undefined)
    const loadUpdates = () => serverApi.updateStatus().then(setUpdates, () => undefined)
    void loadUpdates()
    const timer = setInterval(loadUpdates, 15000)
    return () => clearInterval(timer)
  }, [reload])

  const savePolicy = async () => {
    if (!policy) return
    setSavingPolicy(true)
    try {
      await serverApi.setReleasePolicy(policy.release.id, policy.mode, policy.min.trim())
      message.success('已保存更新方式')
      setPolicy(undefined)
      await reload()
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSavingPolicy(false)
    }
  }

  // 各版本的客户端更新进度计数（按客户端最近一次上报的目标版本）。
  const progressOf = (r: Release) => {
    const os = r.target.includes('darwin') ? 'darwin' : 'windows' // 含 Lite 版（lite-darwin-…）
    const list = updates.filter((u) => u.version === r.version && u.platform.startsWith(os))
    const count = (phases: string[]) => list.filter((u) => phases.includes(u.phase)).length
    const done = updates.filter((u) => u.app_version === r.version && u.platform.startsWith(os)).length
    return { pending: count(['requested', 'checking', 'downloading', 'installing']), installed: count(['installed']), failed: count(['failed']), done }
  }

  const deviceName = (id: number) => devices.find((d) => d.id === id)?.name ?? `#${id}`

  const saveCanary = async () => {
    if (!canary) return
    setSavingCanary(true)
    try {
      await serverApi.setReleaseDevices(canary.release.id, canary.devices)
      message.success(canary.devices.length ? '已设置灰度客户端' : '已改为全量发布')
      setCanary(undefined)
      await reload()
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSavingCanary(false)
    }
  }

  const upload = async (v: UploadForm) => {
    if (!file || !sig) {
      message.error('请选择更新包和签名文件')
      return
    }
    setUploading(true)
    try {
      const signature = (await sig.text()).trim()
      await serverApi.uploadRelease({ ...v, notes: v.notes ?? '', signature, file })
      message.success('已上传，打开「发布」后客户端才会收到')
      form.resetFields(['notes'])
      setFile(undefined)
      setSig(undefined)
      await reload()
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setUploading(false)
    }
  }

  const act = (f: () => Promise<unknown>) =>
    f()
      .catch((e: Error) => message.error(e.message))
      .finally(reload)

  // 每个平台当前全量生效的版本（正式列里最高的那个）。
  const live = new Map<string, string>()
  for (const r of releases.filter(isFormal)) {
    const cur = live.get(r.target)
    if (!cur || compareVersions(r.version, cur) > 0) live.set(r.target, r.version)
  }

  const row = (r: Release) => {
    const p = progressOf(r)
    const formal = isFormal(r)
    return (
      <div key={r.id} className={`release-row${r.published ? '' : ' unpublished'}`}>
        <div className="release-row-head">
          <Tag bordered={false}>{r.target}</Tag>
          {formal ? (
            live.get(r.target) === r.version ? (
              <Tag color="green">全量生效中</Tag>
            ) : (
              <Tag>已被新版本取代</Tag>
            )
          ) : r.published ? (
            <Tag color="orange">灰度中 · {r.devices.length} 台</Tag>
          ) : (
            <Tag>未发布</Tag>
          )}
          <Tag color={r.mode === 'force' ? 'red' : r.mode === 'silent' ? 'blue' : r.mode === 'push' ? 'green' : undefined}>{modeLabels[r.mode] ?? r.mode}</Tag>
          {r.min_version && <span className="hint">最低版本 {r.min_version}</span>}
          <span className="release-row-fill" />
          <Switch
            size="small"
            aria-label={`发布 ${r.version} ${r.target}`}
            checkedChildren="已发布"
            unCheckedChildren="未发布"
            checked={r.published}
            onChange={(on) => act(() => serverApi.publishRelease(r.id, on))}
          />
          <RowAction title="更新方式" icon={<SendOutlined />} onClick={() => setPolicy({ release: r, mode: r.mode ?? 'normal', min: r.min_version ?? '' })} />
          <RowAction title="灰度名单" icon={<ExperimentOutlined />} onClick={() => setCanary({ release: r, devices: r.devices })} />
          {!formal && (
            <Popconfirm
              title={`把 ${r.version}（${r.target}）转为正式版本？`}
              description="清空灰度名单并发布，所有客户端检查更新时都会收到。"
              okText="转为正式"
              cancelText="取消"
              onConfirm={() =>
                act(async () => {
                  await serverApi.setReleaseDevices(r.id, [])
                  if (!r.published) await serverApi.publishRelease(r.id, true)
                  message.success(`${r.version}（${r.target}）已转为正式版本`)
                })
              }
            >
              <span>
                <RowAction title="转为正式" icon={<RiseOutlined />} />
              </span>
            </Popconfirm>
          )}
          <RowDelete confirm={`删除 ${r.version}（${r.target}）？`} onConfirm={() => act(() => serverApi.deleteRelease(r.id))} />
        </div>
        <div className="release-row-meta">
          <span className="selectable" title={r.file_name}>
            {r.file_name}
          </span>
          <span>{size(r.size)}</span>
          <span>上传于 {new Date(r.created_at).toLocaleString()}</span>
          <span>{r.storage === 'oss' ? '存在 OSS' : '存在管理服务'}</span>
        </div>
        {r.notes && <div className="release-row-notes">{r.notes}</div>}
        {r.devices.length > 0 && (
          <Space size={[4, 4]} wrap className="release-row-devices">
            <span className="hint">灰度客户端：</span>
            {r.devices.map((id) => (
              <Tag key={id}>{deviceName(id)}</Tag>
            ))}
          </Space>
        )}
        <div className="hint">
          已是此版本 {p.done} · 进行中 {p.pending} · 待重启 {p.installed}
          {p.failed > 0 && (
            <Tag color="red" style={{ marginLeft: 6 }}>
              失败 {p.failed}
            </Tag>
          )}
        </div>
      </div>
    )
  }

  return (
    <Space orientation="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader title="客户端更新" count={releases.length} />
      <Alert
        type="info"
        showIcon
        title="上传什么"
        description={
          <>
            在构建机上运行 <Typography.Text code>scripts/build-app.sh</Typography.Text>；配置了仓库根目录的 <Typography.Text code>.env</Typography.Text>{' '}
            时打包后自动上传（不发布，出现在左列「未发布」），否则结束时会列出文件，在这里手动上传：macOS 上传
            <Typography.Text code>Banbu Connect.app.tar.gz</Typography.Text>，Windows 上传 <Typography.Text code>-setup.exe</Typography.Text>
            （NSIS），各自配同名的 <Typography.Text code>.sig</Typography.Text> 签名文件。版本号自动从签名读取，须与构建时的应用版本一致（tauri.conf.json 的 version）。
          </>
        }
      />
      <Card title="上传">
        <Form form={form} layout="inline" onFinish={upload} initialValues={{ target: targets[0] }} style={{ rowGap: 12 }}>
          <Form.Item name="version" rules={[{ required: true, pattern: /^\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$/, message: '版本号如 0.2.0' }]}>
            <Input placeholder="版本号，如 0.2.0" style={{ width: 160 }} />
          </Form.Item>
          <Form.Item name="target" rules={[{ required: true, message: '选择平台' }]}>
            <Select placeholder="平台" style={{ width: 180 }} options={targets.map((t) => ({ label: t, value: t }))} />
          </Form.Item>
          <Form.Item name="notes">
            <Input placeholder="更新说明（可选）" style={{ width: 240 }} />
          </Form.Item>
          <Form.Item label="更新包">
            <input type="file" aria-label="更新包" onChange={(e) => setFile(e.target.files?.[0])} />
          </Form.Item>
          <Form.Item label="签名">
            <input
              type="file"
              accept=".sig"
              aria-label="签名文件"
              onChange={(e) => {
                const f = e.target.files?.[0]
                setSig(f)
                // 版本号以签名里的为准（构建时写入），填错了客户端会拒绝安装。
                f?.text().then((t) => {
                  const v = signedVersion(t)
                  if (v) form.setFieldValue('version', v)
                }, () => undefined)
              }}
            />
          </Form.Item>
          <Form.Item>
            <Button type="primary" htmlType="submit" icon={<UploadOutlined />} loading={uploading}>
              上传
            </Button>
          </Form.Item>
        </Form>
      </Card>
      <div className="release-columns">
        <ReleaseColumn
          kind="canary"
          releases={releases.filter((r) => !isFormal(r))}
          loading={loading}
          render={(r) => row(r)}
        />
        <ReleaseColumn kind="formal" releases={releases.filter(isFormal)} loading={loading} render={(r) => row(r)} />
      </div>

      <Card size="small" title={<span className="card-title">各客户端的版本与更新进度</span>}>
        <Table<DeviceUpdate>
          rowKey="device_id"
          size="small"
          dataSource={updates}
          pagination={false}
          scroll={{ x: 820 }}
          columns={[
            { title: '客户端', dataIndex: 'name', width: 160 },
            { title: '平台', dataIndex: 'platform', width: 130, render: (v: string) => v || '—' },
            { title: '当前版本', dataIndex: 'app_version', width: 100, render: (v: string) => v || '—' },
            {
              title: '最近一次更新',
              key: 'phase',
              render: (_, u) => {
                if (!u.phase) return <span className="hint">没有上报（0.2.5 及以前的版本不上报）</span>
                const p = phaseLabels[u.phase] ?? { label: u.phase }
                return (
                  <span>
                    {u.version} <Tag color={p.color}>{p.label}</Tag>
                    {u.mode && u.mode !== 'normal' && <span className="hint">{modeLabels[u.mode as UpdateMode] ?? u.mode}</span>}
                    {u.error && <div className="hint">{u.error}</div>}
                  </span>
                )
              },
            },
            { title: '上报时间', dataIndex: 'updated_at', width: 170, render: (v: string) => (v ? new Date(v).toLocaleString() : '—') },
            { title: '最近联系', dataIndex: 'last_seen', width: 170, render: (v: string) => (v ? new Date(v).toLocaleString() : '—') },
          ]}
        />
      </Card>

      <Modal
        title={policy && `更新方式：${policy.release.version}（${policy.release.target}）`}
        open={!!policy}
        okText="保存"
        cancelText="取消"
        confirmLoading={savingPolicy}
        onOk={savePolicy}
        onCancel={() => setPolicy(undefined)}
        destroyOnHidden
      >
        {policy && (
          <Space orientation="vertical" style={{ width: '100%' }} size={12}>
            <Select<UpdateMode>
              style={{ width: '100%' }}
              value={policy.mode}
              onChange={(mode) => setPolicy({ ...policy, mode })}
              options={[
                { value: 'normal', label: '普通：客户端打开首页时提示，用户点「更新」' },
                { value: 'push', label: '主动推送：客户端后台发现后发系统通知' },
                { value: 'silent', label: '静默更新：后台自动下载安装，退出或空闲时生效' },
                { value: 'force', label: '强制更新：后台自动下载安装；下次打开应用时全屏提示，更新完才能继续使用' },
              ]}
            />
            <Input
              placeholder="最低版本（可不填），如 0.2.6"
              value={policy.min}
              onChange={(e) => setPolicy({ ...policy, min: e.target.value })}
            />
            <Typography.Text type="secondary">
              客户端低于最低版本时也按强制更新处理。全屏强制更新只对高于 0.2.11 的客户端生效（0.2.5–0.2.11 为后台安装后 10 分钟自动重启）；与灰度名单同时生效。
            </Typography.Text>
          </Space>
        )}
      </Modal>

      <Modal
        title={canary && `灰度：${canary.release.version}（${canary.release.target}）`}
        open={!!canary}
        okText="保存"
        cancelText="取消"
        confirmLoading={savingCanary}
        onOk={saveCanary}
        onCancel={() => setCanary(undefined)}
        destroyOnHidden
      >
        <Space orientation="vertical" style={{ width: '100%' }}>
          <Typography.Text type="secondary">
            选了客户端后，只有它们能收到这个版本，其他客户端仍然收到不在灰度中的最高版本。不选即全量发布。还要打开「发布」才会生效。
          </Typography.Text>
          <Select
            mode="multiple"
            allowClear
            style={{ width: '100%' }}
            placeholder="全部客户端"
            optionFilterProp="label"
            value={canary?.devices}
            onChange={(v) => canary && setCanary({ ...canary, devices: v })}
            options={devices
              .filter((d) => !d.revoked || canary?.devices.includes(d.id))
              .map((d) => ({ value: d.id, label: d.app_version ? `${d.name}（当前 ${d.app_version}）` : d.name }))}
          />
        </Space>
      </Modal>
    </Space>
  )
}

// 读 .sig（base64 的 minisign 签名）trusted comment 里的 version 字段，与服务端 dist.ParseSignature 一致。
function signedVersion(sig: string): string | undefined {
  try {
    const text = atob(sig.trim())
    const m = /^trusted comment:.*\bversion:v?([^\t\s]+)/m.exec(text)
    return m?.[1]
  } catch {
    return undefined
  }
}

// 正式版本：已发布且没有灰度名单（所有客户端都能收到）。其余（灰度中、未发布）放左列。
const isFormal = (r: Release) => r.published && r.devices.length === 0

// 比较版本号 x.y.z[-pre]，与服务端 dist.CompareVersions 一致：带预发布后缀的低于同号正式版。
function compareVersions(a: string, b: string): number {
  const split = (v: string) => {
    const i = v.indexOf('-')
    const main = i < 0 ? v : v.slice(0, i)
    return { nums: main.split('.').map((x) => Number(x) || 0), pre: i < 0 ? '' : v.slice(i + 1) }
  }
  const x = split(a)
  const y = split(b)
  for (let i = 0; i < 3; i++) {
    const d = (x.nums[i] ?? 0) - (y.nums[i] ?? 0)
    if (d) return d
  }
  if (x.pre === y.pre) return 0
  if (!x.pre) return 1
  if (!y.pre) return -1
  return x.pre < y.pre ? -1 : 1
}

// 一列更新包：按版本分组、版本从高到低排成时间线，每个版本下列出各平台的包。
function ReleaseColumn({
  kind,
  releases,
  loading,
  render,
}: {
  kind: 'canary' | 'formal'
  releases: Release[]
  loading: boolean
  render: (r: Release) => ReactNode
}) {
  const groups = new Map<string, Release[]>()
  for (const r of releases) groups.set(r.version, [...(groups.get(r.version) ?? []), r])
  const versions = [...groups.keys()].sort((a, b) => compareVersions(b, a))
  const canary = kind === 'canary'
  return (
    <Card
      size="small"
      className="release-column"
      title={
        <span className="card-title">
          {canary ? '灰度与待发布' : '正式版本'}
          <Tag bordered={false}>{versions.length} 个版本</Tag>
        </span>
      }
      extra={
        <span className="hint">
          {canary ? '只发给灰度名单里的客户端；未发布的谁都收不到' : '已发布、发给所有客户端'}
        </span>
      }
    >
      {loading ? (
        <Skeleton active paragraph={{ rows: 4 }} />
      ) : versions.length === 0 ? (
        <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={canary ? '没有灰度中或待发布的更新包' : '还没有正式版本'} />
      ) : (
        <Timeline
          items={versions.map((v) => {
            const list = groups.get(v)!.sort((a, b) => a.target.localeCompare(b.target))
            const latest = list.reduce((t, r) => (r.created_at > t ? r.created_at : t), '')
            const color = canary ? (list.some((r) => r.published) ? 'orange' : 'gray') : 'green'
            return {
              key: v,
              color,
              content: (
                <div className="release-version-body">
                  <div className="release-version">
                    <b>{v}</b>
                    <span className="hint">{new Date(latest).toLocaleString()}</span>
                  </div>
                  {list.map(render)}
                </div>
              ),
            }
          })}
        />
      )}
    </Card>
  )
}
