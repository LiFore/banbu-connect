import { useCallback, useEffect, useState } from 'react'
import { Alert, App, Button, Card, Input, InputNumber, Modal, Skeleton, Switch, Tag, Typography } from 'antd'
import { ApiOutlined, CloudUploadOutlined, ReloadOutlined, SettingOutlined } from '@ant-design/icons'
import { api, type OTelStatus, type OTelSummary } from '../../api'
import { PageHeader } from '../common'
import { DaysPicker, msTime, UsageView } from '../claudeUsage'
import { ExistingList } from './OTelNotice'

// Claude Code 用量：后台服务内置 OTLP 接收器（默认开启），托盘外壳自动把用户的 Claude Code 配置指向它。
// 不收集提示词和助手回复原文；绑定管理服务后上报其余数据。
// 用量页只看统计；接收器的设置不放在显眼处，在「设置」页底部的一行说明里打开（OTelFooter）。
export default function ClaudeUsage() {
  const [summary, setSummary] = useState<OTelSummary>()
  const [days, setDays] = useState(7)
  const [error, setError] = useState<string>()

  const load = useCallback(
    () =>
      api.otel.summary(days).then(
        (sum) => {
          setSummary(sum)
          setError(undefined)
        },
        (e: Error) => setError(e.message),
      ),
    [days],
  )

  useEffect(() => {
    void load()
    const timer = setInterval(load, 15000)
    return () => clearInterval(timer)
  }, [load])

  if (!summary && !error) {
    return (
      <Card>
        <Skeleton active paragraph={{ rows: 6 }} />
      </Card>
    )
  }

  return (
    <div className="desktop-stack">
      <PageHeader
        title="Claude 用量"
        actions={
          <>
            <DaysPicker value={days} onChange={setDays} />
            <Button icon={<ReloadOutlined />} onClick={() => void load()}>
              刷新
            </Button>
          </>
        }
      />
      {error && <Alert type="error" showIcon title={error} />}
      {summary && <UsageView summary={summary} />}
    </div>
  )
}

// 「设置」页底部的一行说明，点里面的链接打开接收器设置。
export function OTelFooter() {
  const [open, setOpen] = useState(false)
  return (
    <p className="hint page-footnote">
      「用量」页的 Claude Code 统计来自本机接收的 OpenTelemetry 数据（不含提示词和助手回复原文），绑定管理服务后会上报给管理员；
      <a role="button" onClick={() => setOpen(true)}>
        数据接收设置
      </a>
      。
      <Modal title="数据接收设置" open={open} footer={null} onCancel={() => setOpen(false)} width={760} destroyOnHidden>
        <OTelSettings />
      </Modal>
    </p>
  )
}

function OTelSettings() {
  const { message } = App.useApp()
  const [status, setStatus] = useState<OTelStatus>()
  const [saving, setSaving] = useState(false)
  const [uploading, setUploading] = useState(false)
  const [error, setError] = useState<string>()

  const load = useCallback(
    () =>
      api.otel.status().then(
        (st) => {
          setStatus(st)
          setError(undefined)
        },
        (e: Error) => setError(e.message),
      ),
    [],
  )

  useEffect(() => {
    void load()
    const timer = setInterval(load, 15000)
    return () => clearInterval(timer)
  }, [load])

  const update = async (patch: Partial<OTelStatus['config']>) => {
    setSaving(true)
    try {
      setStatus(await api.otel.update(patch))
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  const upload = async () => {
    setUploading(true)
    try {
      setStatus(await api.otel.upload())
      message.success('已上报')
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setUploading(false)
    }
  }

  if (error) return <Alert type="error" showIcon title={error} />
  if (!status) return <Skeleton active paragraph={{ rows: 6 }} />
  return (
    // 设置保存后重新挂载，输入框回到已保存的值。
    <ReceiverCard
      key={`${status.config.port}-${status.config.retention_days}`}
      status={status}
      saving={saving}
      uploading={uploading}
      onUpdate={update}
      onUpload={upload}
    />
  )
}

// 手动配置：让托盘程序立即写入（不用等下一次核对），或者把内容复制下来自己粘贴进 settings.json。
function ClaudeConfigActions({ status }: { status: OTelStatus }) {
  const { message } = App.useApp()
  const [syncing, setSyncing] = useState(false)
  const [open, setOpen] = useState(false)
  const json = JSON.stringify({ env: status.manual_env }, null, 2)
  const sync = async () => {
    setSyncing(true)
    const before = status.claude?.at
    try {
      await api.otel.syncClaude()
      // 托盘程序 3 秒内处理，结果回报到状态里。
      for (let i = 0; i < 10; i++) {
        await new Promise((r) => setTimeout(r, 1000))
        const st = await api.otel.status()
        if (st.claude && st.claude.at !== before) {
          if (st.claude.error) message.error(`写入失败：${st.claude.error}`)
          else if (st.claude.configured) message.success('已写入 Claude Code 配置，新开的 Claude Code 会话生效')
          else if (st.claude.existing.length) message.warning('检测到已有的 OTel 配置，没有写入；可以打开开关强制开启，或手动配置')
          else message.info('已核对')
          return
        }
      }
      message.warning('托盘程序没有响应：请确认菜单栏 / 任务栏里的 Banbu Connect Lite 在运行，或使用手动配置')
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSyncing(false)
    }
  }
  return (
    <>
      <Button size="small" loading={syncing} disabled={!status.active} title={status.active ? undefined : '先打开数据接收'} onClick={() => void sync()}>
        立即写入配置
      </Button>
      <Button size="small" onClick={() => setOpen(true)}>
        手动配置
      </Button>
      <Modal title="手动配置 Claude Code" open={open} footer={null} onCancel={() => setOpen(false)} width={640}>
        <Typography.Paragraph>
          把下面的 <code>env</code> 合并进 Claude Code 的用户配置 <code>~/.claude/settings.json</code>（Windows 为{' '}
          <code>%USERPROFILE%\.claude\settings.json</code>）；已有 <code>env</code> 时只加入这些键。保存后新开的 Claude Code 会话生效。
        </Typography.Paragraph>
        <Typography.Paragraph type="secondary">
          不收集提示词和助手回复原文。手动写入的配置不会被本客户端自动删除，关闭数据接收后请自行删掉这些键。
        </Typography.Paragraph>
        <Input.TextArea className="mono-input selectable" readOnly autoSize value={json} />
        <div style={{ marginTop: 12, textAlign: 'right' }}>
          <Button
            type="primary"
            onClick={() =>
              navigator.clipboard.writeText(json).then(
                () => message.success('已复制'),
                () => message.error('复制失败，请手动选择'),
              )
            }
          >
            复制
          </Button>
        </div>
      </Modal>
    </>
  )
}

function ReceiverCard({
  status,
  saving,
  uploading,
  onUpdate,
  onUpload,
}: {
  status: OTelStatus
  saving: boolean
  uploading: boolean
  onUpdate: (patch: Partial<OTelStatus['config']>) => void
  onUpload: () => void
}) {
  const [port, setPort] = useState(status.config.port)
  const [retention, setRetention] = useState(status.config.retention_days)
  const { claude, config, upload } = status
  const existing = claude?.existing ?? []

  let claudeTag = <Tag>等待托盘程序配置</Tag>
  if (status.suppressed) claudeTag = <Tag color="warning">已有 OTel 配置，未自动开启</Tag>
  else if (!status.active) claudeTag = <Tag>未开启</Tag>
  else if (claude?.error) claudeTag = <Tag color="error">配置失败</Tag>
  else if (claude?.configured) claudeTag = <Tag color="success">已配置</Tag>
  else if (existing.length) claudeTag = <Tag color="warning">与已有配置冲突</Tag>

  return (
    <Card
      title={
        <span className="card-title">
          <ApiOutlined />
          Claude Code 数据接收
        </span>
      }
      extra={<Switch checked={status.active} loading={saving} onChange={(enabled) => onUpdate({ enabled })} />}
    >
      <p className="hint">
        开启后，本机接收 Claude Code 的 OpenTelemetry 数据（用量、费用、工具调用等），并自动在你的 Claude Code 配置（settings.json 的
        env）里指向这里，对之后新开的 Claude Code 会话生效。<b>不收集提示词和助手回复的原文</b>；绑定管理服务后，其余数据会上报给管理员。
      </p>
      <div className="setting-row">
        <span>接收器</span>
        <span>
          {status.listening ? <Tag color="success">运行中</Tag> : <Tag>{status.active ? '未运行' : '已关闭'}</Tag>}
          <code className="selectable">{status.endpoint}</code>
        </span>
      </div>
      {status.error && <Alert type="error" showIcon title={status.error} />}
      <div className="setting-row">
        <span>Claude Code 配置</span>
        <span>
          {claudeTag}
          {claude?.settings_path && <code className="selectable">{claude.settings_path}</code>}
          <ClaudeConfigActions status={status} />
        </span>
      </div>
      {status.active && claude?.error && <Alert type="error" showIcon title={claude.error} />}
      {existing.length > 0 && (
        <Alert
          type={status.suppressed ? 'warning' : 'info'}
          showIcon
          title={
            status.suppressed
              ? '这台电脑上已经配置过 Claude Code 的 OpenTelemetry，为避免改动你的配置，没有自动开启。打开右上角的开关即可开启（会覆盖 settings.json 里的同名配置，原值保留，关闭时恢复）。'
              : '这台电脑上还有以下 OpenTelemetry 配置（不是本客户端写入的）：'
          }
          description={<ExistingList existing={existing} />}
        />
      )}
      <div className="setting-row">
        <span className="card-title">
          <CloudUploadOutlined />
          上报到管理服务
        </span>
        <span>
          {!upload.bound ? (
            <Tag>未绑定管理服务</Tag>
          ) : upload.last_error ? (
            <Tag color="error" title={upload.last_error}>
              上报失败
            </Tag>
          ) : (
            <Tag color="success">{upload.last_at ? `最近 ${new Date(upload.last_at).toLocaleString()}` : '已绑定'}</Tag>
          )}
          <span className="hint">待上报 {upload.pending} 条</span>{' '}
          <Button size="small" loading={uploading} disabled={!status.active || upload.pending === 0} onClick={onUpload}>
            立即上报
          </Button>
        </span>
      </div>
      {upload.last_error && <Alert type="error" showIcon title={`上报失败：${upload.last_error}`} />}
      <div className="setting-row">
        <span className="card-title">
          <SettingOutlined />
          端口 / 本机保留天数
        </span>
        <span>
          <InputNumber aria-label="接收端口" min={1024} max={65535} value={port} onChange={(v) => setPort(v ?? config.port)} />{' '}
          <InputNumber
            aria-label="保留天数"
            min={0}
            max={3650}
            value={retention}
            addonAfter="天"
            onChange={(v) => setRetention(v ?? config.retention_days)}
          />{' '}
          <Button
            disabled={port === config.port && retention === config.retention_days}
            loading={saving}
            onClick={() => onUpdate({ port, retention_days: retention })}
          >
            保存
          </Button>
        </span>
      </div>
      <p className="hint">
        已收到：事件 {status.counts.logs}、指标 {status.counts.metrics}、trace {status.counts.spans}
        {status.counts.last_received_ms ? `，最近一次 ${msTime(status.counts.last_received_ms)}` : '，还没有收到数据'}。保留天数为 0 表示不清理。
      </p>
    </Card>
  )
}
