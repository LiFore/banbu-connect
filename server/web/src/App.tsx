import { useEffect, useState } from 'react'
import { App as AntApp, Card, ConfigProvider, Spin, Typography } from 'antd'
import { api, UNAUTHENTICATED_EVENT, type SessionInfo } from './api'
import Login from './pages/Login'
import ChangePasswordForm from './pages/ChangePassword'
import { desktopTheme } from './pages/server/theme'
import ServerShell from './pages/server/ServerShell'

const inAdmin = () => location.pathname === '/admin' || location.pathname.startsWith('/admin/')

const loggedOut: SessionInfo = { authenticated: false, must_change: false }

export default function App() {
  const [session, setSession] = useState<SessionInfo>()

  // 退出登录后仍要知道是不是管理服务，登录页和配色按它区分。
  const signOut = () => setSession((s) => ({ ...loggedOut, server: s?.server }))

  useEffect(() => {
    api.session().then(setSession, () => setSession(loggedOut))
    const onUnauth = () => setSession((s) => ({ ...loggedOut, server: s?.server }))
    window.addEventListener(UNAUTHENTICATED_EVENT, onUnauth)
    return () => window.removeEventListener(UNAUTHENTICATED_EVENT, onUnauth)
  }, [])

  // 管理服务沿用 Banbu Connect 客户端的令牌（desktop.css 的 html.server-mode）。
  useEffect(() => {
    document.documentElement.classList.add('server-mode')
    document.title = 'Banbu Connect 管理服务'
  }, [])

  if (!session) {
    return <Spin fullscreen />
  }
  if (!inAdmin()) {
    // 旧地址（如 /publish）跳到 /admin 下；根路径是落地页，不会走到这里。
    location.replace(`/admin${location.pathname}${location.search}`)
    return <Spin fullscreen />
  }
  return (
    <ConfigProvider theme={desktopTheme} button={{ autoInsertSpace: false }}>
      <AntApp>
        {!session.authenticated ? (
          <Login server onLogin={setSession} />
        ) : session.must_change ? (
          <MustChange onDone={setSession} />
        ) : (
          <ServerShell onSession={setSession} onLogout={() => void api.logout().finally(signOut)} />
        )}
      </AntApp>
    </ConfigProvider>
  )
}

function MustChange({ onDone }: { onDone: (s: SessionInfo) => void }) {
  return (
    <div style={{ minHeight: '100vh', display: 'grid', placeItems: 'center', padding: 16 }}>
      <Card style={{ width: 400, maxWidth: '100%' }}>
        <Typography.Title level={3}>首次登录：请修改密码</Typography.Title>
        <ChangePasswordForm onDone={onDone} />
      </Card>
    </div>
  )
}
