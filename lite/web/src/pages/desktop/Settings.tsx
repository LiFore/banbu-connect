import { useCallback, useEffect, useState } from 'react'
import { Card, Empty, Tag } from 'antd'
import { HistoryOutlined } from '@ant-design/icons'
import { api, type DesktopEvent } from '../../api'
import ServerBinding from './ServerBinding'
import EgressCard from './EgressCard'
import { OTelFooter } from './ClaudeUsage'
import { AppUpdateBanner, AppVersionLine, useAppUpdate } from './AppUpdate'

// 设置：管理服务绑定（接入申请）、应用更新、Claude Code 遥测的接收设置、最近的事件。
export default function Settings() {
  const update = useAppUpdate()
  return (
    <div className="desktop-stack">
      <AppUpdateBanner update={update} />
      <ServerBinding />
      <EgressCard />
      <EventsCard />
      <AppVersionLine update={update} />
      <OTelFooter />
    </div>
  )
}

const levelColor: Record<string, string> = { error: 'red', warn: 'orange', info: 'default' }

// 后台事件：更新失败、令牌续期提醒等，新的在前。
function EventsCard() {
  const [events, setEvents] = useState<DesktopEvent[]>()
  const load = useCallback(() => {
    api.desktop.events().then(setEvents, () => setEvents([]))
  }, [])
  useEffect(() => {
    load()
    const timer = setInterval(load, 10000)
    return () => clearInterval(timer)
  }, [load])

  return (
    <Card
      title={
        <span className="card-title">
          <HistoryOutlined />
          最近事件
        </span>
      }
    >
      {!events || events.length === 0 ? (
        <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="没有事件" />
      ) : (
        <ul className="simulate-list">
          {events.slice(0, 30).map((e, i) => (
            <li key={`${e.time}-${i}`}>
              <Tag color={levelColor[e.level] ?? 'default'}>{new Date(e.time).toLocaleString()}</Tag>
              <span className="selectable">{e.message}</span>
            </li>
          ))}
        </ul>
      )}
    </Card>
  )
}
