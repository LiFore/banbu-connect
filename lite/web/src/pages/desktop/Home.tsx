import { useCallback, useEffect, useState } from 'react'
import { Alert } from 'antd'
import { Link } from 'react-router-dom'
import { api, type DesktopSummary } from '../../api'
import Accounts from './Accounts'
import { AppUpdateBanner, AppVersionLine, useAppUpdate } from './AppUpdate'

// 首页就是账号页：上面是连接管理服务的提示和应用更新，下面是账号卡片。
export default function Home() {
  const update = useAppUpdate()
  const [status, setStatus] = useState<DesktopSummary>()
  const [error, setError] = useState<string>()

  const load = useCallback(() => {
    api.desktop.status().then(
      (s) => {
        setStatus(s)
        setError(undefined)
      },
      (e: Error) => setError(e.message),
    )
  }, [])

  useEffect(() => {
    load()
    const timer = setInterval(load, 5000)
    return () => clearInterval(timer)
  }, [load])

  const server = status?.server
  return (
    <div className="desktop-stack">
      {error && <Alert type="error" showIcon title="连不上后台服务" description={error} />}
      {server && !server.bound && (
        <Alert
          type="info"
          showIcon
          title={
            <>
              还没有绑定管理服务，账号由管理员分配：<Link to="/settings">去绑定</Link>
            </>
          }
        />
      )}
      {server?.bound && server.last_error && (
        <Alert type="warning" showIcon title="管理服务同步失败，账号列表可能不是最新的" description={server.last_error} />
      )}
      <AppUpdateBanner update={update} />
      <Accounts embedded />
      <AppVersionLine update={update} />
    </div>
  )
}
