import { useEffect, useState } from 'react'
import { App as AntApp, Card, ConfigProvider, Spin, Typography } from 'antd'
import { api, UNAUTHENTICATED_EVENT, type SessionInfo } from './api'
import DesktopShell from './pages/desktop/DesktopShell'
import { desktopTheme } from './pages/desktop/theme'

// Banbu Connect Lite 只有桌面客户端一种界面：页面由本机后台进程提供，令牌由托盘外壳带进来。
export default function App() {
  const [session, setSession] = useState<SessionInfo>()

  useEffect(() => {
    api.session().then(setSession, () => setSession({ authenticated: false }))
    const onUnauth = () => setSession({ authenticated: false })
    window.addEventListener(UNAUTHENTICATED_EVENT, onUnauth)
    return () => window.removeEventListener(UNAUTHENTICATED_EVENT, onUnauth)
  }, [])

  if (!session) {
    return <Spin fullscreen />
  }
  if (!session.authenticated) {
    return (
      <div style={{ minHeight: '100vh', display: 'grid', placeItems: 'center', padding: 16 }}>
        <Card style={{ width: 400, maxWidth: '100%' }}>
          <Typography.Title level={4}>无法访问</Typography.Title>
          <Typography.Paragraph>请从 Banbu Connect Lite 桌面客户端打开这个页面。</Typography.Paragraph>
        </Card>
      </div>
    )
  }
  return (
    <ConfigProvider theme={desktopTheme} button={{ autoInsertSpace: false }}>
      {/* 提示条放到标题栏（44px，--box-titlebar-h）下面的内容区里，不挡标题栏 */}
      <AntApp message={{ top: 56 }}>
        <DesktopShell />
      </AntApp>
    </ConfigProvider>
  )
}
