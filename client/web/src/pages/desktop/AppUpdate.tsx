import { useCallback, useEffect, useState } from 'react'
import { Alert, App, Button, Card, Progress, Space, Typography } from 'antd'
import { CloudDownloadOutlined, ReloadOutlined, SyncOutlined } from '@ant-design/icons'
import { api, type AppUpdateStatus } from '../../api'

// 首页的应用更新：后台服务查管理服务有没有新版本；点「更新」后由托盘程序下载、校验、安装，
// 进度经后台服务回报到这里（见 internal/desktop/appupdate.go）；装完提示重启。

export const ACTIVE = new Set(['requested', 'checking', 'downloading', 'installing'])
// 请求发出后托盘程序 3 秒内认领；超过这个时间还没动静，多半是托盘程序没在运行。
export const STALL_MS = 15000

export const phaseText: Record<string, string> = {
  requested: '已请求，等待托盘程序开始…',
  checking: '正在确认更新…',
  downloading: '正在下载…',
  installing: '正在安装…',
}

export const mb = (n: number) => `${(n / 1024 / 1024).toFixed(1)} MB`

// 首页顶部条和底部版本行共用一份状态（在 Home 里调用一次），否则底部点「检查更新」后顶部条要等下次轮询才更新。
export function useAppUpdate() {
  const [status, setStatus] = useState<AppUpdateStatus>()
  const [error, setError] = useState<string>()
  const load = useCallback(
    (refresh = false) =>
      api.appUpdate.status(refresh).then(
        (s) => {
          setStatus(s)
          setError(undefined)
        },
        (e: Error) => setError(e.message),
      ),
    [],
  )
  const active = !!status?.progress && ACTIVE.has(status.progress.phase)
  useEffect(() => {
    void load()
    // 进行中每秒刷新进度；平时每分钟（后台服务自己每 30 分钟检查，静默更新可能随时开始）。
    const timer = setInterval(() => void load(), active ? 1000 : 60000)
    return () => clearInterval(timer)
  }, [load, active])
  return { status, setStatus, error, load, active }
}

// 首页顶部：有新版本、正在更新、装完待重启或失败时显示。
export type AppUpdateState = ReturnType<typeof useAppUpdate>

export function AppUpdateBanner({ update }: { update: AppUpdateState }) {
  const { message } = App.useApp()
  const { status, setStatus, active } = update
  const [busy, setBusy] = useState(false)
  const [restarting, setRestarting] = useState(false)
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!active) return
    const t = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(t)
  }, [active])

  if (!status) return null
  const p = status.progress
  const run = async (f: () => Promise<AppUpdateStatus>) => {
    setBusy(true)
    try {
      setStatus(await f())
      return true
    } catch (e) {
      message.error((e as Error).message)
      return false
    } finally {
      setBusy(false)
    }
  }

  if (p && ACTIVE.has(p.phase)) {
    const stalled = p.phase === 'requested' && now - Date.parse(p.updated_at) > STALL_MS
    const percent = p.total > 0 ? Math.min(100, Math.round((p.downloaded / p.total) * 100)) : undefined
    return (
      <Card size="small">
        <Space orientation="vertical" style={{ width: '100%' }}>
          <Typography.Text strong>
            <SyncOutlined spin /> {status.auto ? '正在后台更新到' : '正在更新到'} {p.version || status.available?.version}
            {status.auto && (status.available?.mode === 'force' ? '（强制更新）' : '（静默更新）')}
          </Typography.Text>
          <Progress
            percent={p.phase === 'installing' ? 100 : (percent ?? 0)}
            status="active"
            showInfo={percent !== undefined || p.phase === 'installing'}
          />
          <Typography.Text type="secondary">
            {phaseText[p.phase]}
            {p.phase === 'downloading' && p.downloaded > 0 && ` ${mb(p.downloaded)}${p.total ? ` / ${mb(p.total)}` : ''}`}
          </Typography.Text>
          {stalled && (
            <Alert
              type="warning"
              showIcon
              title="托盘程序没有响应"
              description="请确认菜单栏 / 任务栏里的图标在运行"
            />
          )}
        </Space>
      </Card>
    )
  }
  if (p?.phase === 'installed') {
    return (
      <Alert
        type="success"
        showIcon
        title={restarting ? '正在重启应用…' : `${p.version ?? '新版本'} 已安装，重启后生效`}
        description={!restarting && (status.available?.mode === 'force' || status.auto) ? '空闲时会自动重启' : undefined}
        action={
          <Button
            type="primary"
            loading={busy || restarting}
            onClick={() => {
              setRestarting(true)
              void run(api.appUpdate.restart).then((ok) => setRestarting(ok))
            }}
          >
            立即重启
          </Button>
        }
      />
    )
  }
  if (p?.phase === 'failed') {
    return (
      <Alert
        type="error"
        showIcon
        title={`更新${p.version ? `到 ${p.version} ` : ''}失败`}
        description={p.error || '未知原因'}
        action={
          <Button loading={busy} onClick={() => void run(api.appUpdate.install)}>
            重试
          </Button>
        }
      />
    )
  }
  const a = status.available
  if (!a) return null
  const force = a.mode === 'force'
  return (
    <Alert
      type={force ? 'warning' : 'info'}
      showIcon
      icon={<CloudDownloadOutlined />}
      title={
        <span title={a.notes || undefined}>
          {force ? `需更新到 ${a.version}（管理员要求）` : `新版本 ${a.version}`}
          <span className="hint"> · 当前 {status.current}</span>
        </span>
      }
      action={
        <Button type="primary" loading={busy} onClick={() => void run(api.appUpdate.install)}>
          更新
        </Button>
      }
    />
  )
}

// 首页底部：当前版本和手动检查。
export function AppVersionLine({ update }: { update: AppUpdateState }) {
  const { message } = App.useApp()
  const { status, error, load } = update
  const [checking, setChecking] = useState(false)
  const check = async () => {
    setChecking(true)
    await load(true)
    setChecking(false)
    message.info('已检查更新')
  }
  let text = '正在检查更新…'
  if (error) text = `检查更新失败：${error}`
  else if (status && !status.bound) text = '未绑定管理服务，不能在线更新'
  else if (status?.check_error) text = `检查更新失败：${status.check_error}`
  else if (status?.available) text = `有新版本 ${status.available.version}`
  else if (status) text = '已是最新版本'
  return (
    <div className="hint" style={{ display: 'flex', alignItems: 'center', gap: 8, justifyContent: 'center' }}>
      <span>
        Banbu Connect {status?.current ?? ''} · {text}
        {status?.checked_at && !error ? `（${new Date(status.checked_at).toLocaleTimeString()}）` : ''}
      </span>
      <Button size="small" type="link" icon={<ReloadOutlined />} loading={checking} onClick={() => void check()}>
        检查更新
      </Button>
    </div>
  )
}
