import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { Alert, Button, Progress } from 'antd'
import { CheckCircleFilled, CloudDownloadOutlined, ExclamationCircleFilled } from '@ant-design/icons'
import { api, type AppUpdateStatus } from '../../api'
import logo from '../../assets/logo.png'
import { ACTIVE, mb, phaseText, STALL_MS } from './AppUpdate'
import { WindowControls, useWindowPlatform } from './windowChrome'

// 强制更新的全屏拦截（管理员发布为「强制更新」，或客户端低于最低版本，见 internal/dist/update_policy.go）。
// 只在「打开应用」时拦：页面刚加载（应用启动），或外壳重新显示窗口（托盘点「打开主界面」，外壳发 banbu-shown 事件）。
// 用着用着收到强制更新不打断，首页顶部提示即可；后台服务已经在后台自动下载安装。
// 拦住后：没装就请托盘程序安装，装好后几秒内自动重启应用，重启后版本够了，这一层就不再出现。

const RESTART_DELAY = 3 // 装好后几秒自动重启
const RESTART_STUCK_MS = 20000 // 发出重启后这么久窗口还在，提示手动重启

const isForce = (s?: AppUpdateStatus) => s?.available?.mode === 'force'

export default function ForceUpdateGate() {
  const [status, setStatus] = useState<AppUpdateStatus>()
  const [armed, setArmed] = useState(false)
  const [error, setError] = useState<string>()
  const [now, setNow] = useState(() => Date.now())
  const [readyAt, setReadyAt] = useState<number>() // 看到安装完成的时间，从这时起倒数
  const [restartAt, setRestartAt] = useState<number>() // 发出重启请求的时间
  const requested = useRef<string>(undefined) // 已为哪个版本自动请求过安装
  const ready = useRef<number>(undefined)
  const restarting = useRef(false)
  const platform = useWindowPlatform()

  const install = useCallback(() => {
    setError(undefined)
    api.appUpdate.install().then(setStatus, (e: Error) => setError(e.message))
  }, [])

  const restart = useCallback(() => {
    restarting.current = true
    setRestartAt(Date.now())
    api.appUpdate.restart().then(setStatus, (e: Error) => {
      restarting.current = false
      setRestartAt(undefined)
      setError(e.message)
    })
  }, [])

  // 每拿到一次状态推进一步：还没开始就请托盘程序安装（后台服务的自动安装在跑就不重复请求，每个版本自动请求一次，
  // 失败后由用户点重试）；装好后倒数几秒自动重启。
  const step = useCallback(
    (s: AppUpdateStatus) => {
      setStatus(s)
      const a = s.available
      if (!a || a.mode !== 'force') return
      const p = s.progress
      const mine = !!p && (!p.version || p.version === a.version)
      const phase = mine ? p.phase : undefined
      if (phase === 'installed') {
        const t = Date.now()
        if (ready.current === undefined) {
          ready.current = t
          setReadyAt(t)
        }
        if (!restarting.current && t - ready.current >= RESTART_DELAY * 1000) restart()
        return
      }
      ready.current = undefined
      setReadyAt(undefined)
      if (phase === 'failed' || (phase && ACTIVE.has(phase)) || requested.current === a.version) return
      requested.current = a.version
      install()
    },
    [install, restart],
  )

  // 打开应用时查一次（绕过后台服务的检查缓存），需要强制更新就拦住。
  const arm = useCallback(() => {
    api.appUpdate.status(true).then(
      (s) => {
        if (!isForce(s)) return
        setArmed(true)
        step(s)
      },
      () => undefined,
    )
  }, [step])

  useEffect(() => {
    arm()
    const onShown = () => arm()
    const onVisible = () => {
      if (document.visibilityState === 'visible') arm()
    }
    window.addEventListener('banbu-shown', onShown)
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      window.removeEventListener('banbu-shown', onShown)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [arm])

  // 拦住期间每秒刷新进度。
  useEffect(() => {
    if (!armed) return
    const t = setInterval(() => {
      setNow(Date.now())
      api.appUpdate.status().then(
        (s) => {
          setError(undefined)
          if (!isForce(s)) setArmed(false) // 管理员撤回了强制更新
          step(s)
        },
        (e: Error) => setError(e.message),
      )
    }, 1000)
    return () => clearInterval(t)
  }, [armed, step])

  const a = status?.available
  const p = status?.progress
  const mine = !!p && !!a && (!p.version || p.version === a.version)
  const installed = mine && p?.phase === 'installed'
  const failed = mine && p?.phase === 'failed'
  const active = mine && !!p && ACTIVE.has(p.phase)
  const left = readyAt ? Math.max(0, RESTART_DELAY - Math.floor((now - readyAt) / 1000)) : RESTART_DELAY

  if (!armed || !a) return null

  const percent = p && p.total > 0 ? Math.min(100, Math.round((p.downloaded / p.total) * 100)) : undefined
  const stalled = active && p?.phase === 'requested' && now - Date.parse(p.updated_at) > STALL_MS
  const restartStuck = !!restartAt && now - restartAt > RESTART_STUCK_MS

  let icon = <CloudDownloadOutlined className="force-update-icon" />
  let title = `正在更新到 ${a.version}`
  let body: ReactNode = (
    <>
      <Progress percent={p?.phase === 'installing' ? 100 : (percent ?? 0)} status="active" showInfo={percent !== undefined || p?.phase === 'installing'} />
      <div className="hint">
        {p ? phaseText[p.phase] ?? '正在准备…' : '正在准备…'}
        {p?.phase === 'downloading' && p.downloaded > 0 && ` ${mb(p.downloaded)}${p.total ? ` / ${mb(p.total)}` : ''}`}
      </div>
    </>
  )
  if (installed) {
    icon = <CheckCircleFilled className="force-update-icon force-update-ok" />
    title = `${a.version} 已安装`
    body = (
      <>
        <div className="hint">
          {restartAt ? '正在重启应用…窗口会关闭并重新打开；如果提示更新后台服务，请按提示授权。' : `${left} 秒后自动重启应用完成更新`}
        </div>
        <Button type="primary" size="large" loading={!!restartAt && !restartStuck} onClick={restart}>
          {restartStuck ? '重新尝试重启' : '立即重启'}
        </Button>
        {restartStuck && <div className="hint">还没有重启：请在托盘菜单里选「退出」，再重新打开 Banbu Connect。</div>}
      </>
    )
  } else if (failed) {
    icon = <ExclamationCircleFilled className="force-update-icon force-update-bad" />
    title = `更新到 ${a.version} 失败`
    body = (
      <>
        <div className="hint selectable">{p?.error || '未知原因'}。托盘菜单「查看日志」里有详细记录。</div>
        <Button type="primary" size="large" onClick={install}>
          重试
        </Button>
      </>
    )
  }

  return (
    <div className="force-update" role="alertdialog" aria-modal="true" aria-label="需要更新" data-tauri-drag-region>
      {platform === 'win' && (
        <div className="force-update-controls">
          <WindowControls />
        </div>
      )}
      <div className="force-update-card">
        <img src={logo} alt="" className="force-update-logo" />
        <div className="force-update-kicker">需要更新 Banbu Connect</div>
        <h1 className="force-update-title">
          {icon}
          {title}
        </h1>
        <p className="force-update-desc">
          管理员要求更新到新版本后才能继续使用（当前 {status?.current}）。更新只重启应用窗口和托盘，代理和保护服务不受影响，设置和数据都会保留。
        </p>
        <div className="force-update-body">{body}</div>
        {a.notes && (
          <details className="force-update-notes">
            <summary>更新说明</summary>
            <div className="selectable">{a.notes}</div>
          </details>
        )}
        {stalled && (
          <Alert
            type="warning"
            showIcon
            title="托盘程序没有响应"
            description="更新由托盘程序（菜单栏 / 任务栏里的 Banbu Connect 图标）下载安装。请确认它在运行，或退出后重新打开 Banbu Connect。"
          />
        )}
        {error && <Alert type="error" showIcon title={error} />}
      </div>
    </div>
  )
}
