import { useEffect, useState } from 'react'
import { App as AntApp, Card, ConfigProvider, Spin, Typography } from 'antd'
import { api, UNAUTHENTICATED_EVENT, type SessionInfo } from './api'
import Login from './pages/Login'
import ChangePasswordForm from './pages/ChangePassword'
import BoxShell from './pages/box/BoxShell'
import { boxTheme } from './pages/box/theme'

const loggedOut: SessionInfo = { authenticated: false, must_change: false }

export default function App() {
  const [session, setSession] = useState<SessionInfo>()

  const signOut = () => setSession(loggedOut)

  useEffect(() => {
    api.session().then(setSession, () => setSession(loggedOut))
    const onUnauth = () => setSession(loggedOut)
    window.addEventListener(UNAUTHENTICATED_EVENT, onUnauth)
    return () => window.removeEventListener(UNAUTHENTICATED_EVENT, onUnauth)
  }, [])

  // 路由盒沿用 Banbu Connect 客户端的令牌（desktop.css 的 html.server-mode），另加 box-mode。
  useEffect(() => {
    document.documentElement.classList.add('server-mode', 'box-mode')
    document.title = 'Banbu Box'
  }, [])

  if (!session) {
    return <Spin fullscreen />
  }
  // 路由盒（iStoreOS）面板：与 Banbu Connect 客户端同款界面。
  return (
    <ConfigProvider theme={boxTheme} button={{ autoInsertSpace: false }}>
      <AntApp>
        {!session.authenticated ? (
          <Login box onLogin={setSession} />
        ) : session.must_change ? (
          <MustChange onDone={setSession} />
        ) : (
          <BoxShell onSession={setSession} onLogout={() => void api.logout().finally(signOut)} />
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
