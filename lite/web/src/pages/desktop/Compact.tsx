import { useCallback, useEffect, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { Alert, App, Button } from 'antd'
import { ExpandOutlined, PoweroffOutlined } from '@ant-design/icons'
import { api } from '../../api'
import logo from '../../assets/logo.png'
import { WindowControls, inShell, openFullWindow, quitApp, useWindowPlatform } from './windowChrome'
import ForceUpdateGate from './ForceUpdate'
import Accounts from './Accounts'
import { StartupEgressCheck } from './EgressGuard'

// 精简主界面：主窗口默认显示它（外壳打开 /compact），和完整界面是两个页面。
// 上面是账号（和完整界面首页同一套卡片，一排横向滚动），底部是「完整界面」和「退出」。
export default function Compact() {
  const platform = useWindowPlatform()
  const navigate = useNavigate()
  const { message, modal } = App.useApp()
  const [error, setError] = useState<string>()
  const [unbound, setUnbound] = useState(false)

  const load = useCallback(() => {
    api.desktop.status().then(
      (s) => {
        setError(undefined)
        setUnbound(!s.server.bound)
      },
      (e: Error) => setError(e.message),
    )
  }, [])

  // 状态 5 秒取一次；窗口藏到后台时停下。
  useEffect(() => {
    let timer: ReturnType<typeof setInterval> | undefined
    const start = () => {
      if (timer) return
      load()
      timer = setInterval(load, 5000)
    }
    const stop = () => {
      clearInterval(timer)
      timer = undefined
    }
    const onVis = () => (document.visibilityState === 'visible' ? start() : stop())
    start()
    document.addEventListener('visibilitychange', onVis)
    return () => {
      stop()
      document.removeEventListener('visibilitychange', onVis)
    }
  }, [load])

  // 没有外壳（在浏览器里预览）时在当前页跳到完整界面。
  const openFull = (path?: string) => {
    if (!openFullWindow(path)) navigate(path ?? '/')
  }

  const askQuit = useCallback(() => {
    modal.confirm({
      title: '退出 Banbu Connect Lite？',
      content: '后台进程一起退出，托管账号的令牌同步和额度查询会暂停，下次打开应用时恢复。已经打开的 Claude Code、Codex 等不受影响。',
      okText: '退出',
      okButtonProps: { danger: true },
      cancelText: '取消',
      onOk: () => {
        if (!quitApp()) message.info('请在桌面客户端里退出')
      },
    })
  }, [message, modal])

  // 托盘的「退出」也在这里确认：外壳带 ?quit=1 打开精简页，或已在精简页时发 banbu-quit 事件。
  const [search, setSearch] = useSearchParams()
  useEffect(() => {
    if (search.get('quit') === '1') askQuit()
    if (search.has('quit')) setSearch((p) => (p.delete('quit'), p), { replace: true })
  }, [search, setSearch, askQuit])
  useEffect(() => {
    window.addEventListener('banbu-quit', askQuit)
    return () => window.removeEventListener('banbu-quit', askQuit)
  }, [askQuit])

  // Windows 右上角是窗口按钮，应用名放左边；macOS 红绿灯在左，应用名居中。
  const brand = (
    <div className="desktop-logo" data-tauri-drag-region>
      <img src={logo} alt="" data-tauri-drag-region />
      <span data-tauri-drag-region>Banbu Connect Lite</span>
    </div>
  )

  return (
    <div className="desktop-shell compact-shell">
      <header className="desktop-titlebar desktop-titlebar-centered" data-platform={platform} data-tauri-drag-region>
        {platform === 'win' ? (
          <>
            {brand}
            <div className="titlebar-side titlebar-right" data-tauri-drag-region>
              <WindowControls />
            </div>
          </>
        ) : (
          <>
            <div className="titlebar-side" data-tauri-drag-region />
            {brand}
            <div className="titlebar-side titlebar-right" data-tauri-drag-region />
          </>
        )}
      </header>

      <main className="compact-body">
        {error && <Alert type="error" showIcon title="连不上后台服务" description={error} />}
        {unbound && (
          <Alert
            type="info"
            showIcon
            title={
              <>
                还没有绑定管理服务：
                <a role="button" onClick={() => openFull('/settings')}>
                  去绑定
                </a>
              </>
            }
          />
        )}
        <Accounts compact />
      </main>

      <footer className="compact-footer">
        <Button icon={<ExpandOutlined />} onClick={() => openFull()}>
          完整界面
        </Button>
        {inShell() && (
          <Button danger icon={<PoweroffOutlined />} onClick={askQuit}>
            退出
          </Button>
        )}
      </footer>
      <ForceUpdateGate />
      <StartupEgressCheck />
    </div>
  )
}
