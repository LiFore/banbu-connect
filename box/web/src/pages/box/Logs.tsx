import { useCallback, useEffect, useState } from 'react'
import { Alert, Button, Card, Empty, Segmented, Select, Skeleton, Tag } from 'antd'
import { DownloadOutlined, ReloadOutlined } from '@ant-design/icons'
import { api, type LogEntry } from '../../api'
import { PageHeader } from '../common'

const sources = [
  { value: 'panel', label: '面板' },
  { value: 'guard', label: 'guard' },
  { value: 'core', label: '内核' },
]

const levelColor: Record<string, string> = { ERROR: 'error', WARN: 'warning', INFO: 'blue', DEBUG: 'default' }

// 日志查看（M11-03）：面板、guard 各自最近 2000 条（已脱敏），内核取日志文件末尾；诊断包一键导出（M11-04）。
export default function Logs() {
  const [source, setSource] = useState('panel')
  const [level, setLevel] = useState('INFO')
  const [list, setList] = useState<LogEntry[]>()
  const [error, setError] = useState<string>()

  const load = useCallback(() => {
    api.box.logs(source, level).then(
      (l) => {
        setList(l.slice().reverse())
        setError(undefined)
      },
      (e: Error) => setError(e.message),
    )
  }, [source, level])

  useEffect(() => {
    load()
    const t = setInterval(load, 5000)
    return () => clearInterval(t)
  }, [load])

  return (
    <div className="desktop-stack">
      <PageHeader
        title="日志"
        actions={
          <>
            <Button icon={<ReloadOutlined />} onClick={load}>
              刷新
            </Button>
            <Button icon={<DownloadOutlined />} href={api.box.diagnosticsURL()}>
              导出诊断包
            </Button>
          </>
        }
      />
      <Card
        title={<Segmented value={source} onChange={(v) => setSource(v as string)} options={sources} />}
        extra={
          <Select
            size="small"
            value={level}
            onChange={setLevel}
            style={{ width: 120 }}
            options={[
              { value: '', label: '全部级别' },
              { value: 'INFO', label: 'INFO 及以上' },
              { value: 'WARN', label: 'WARN 及以上' },
              { value: 'ERROR', label: '只看 ERROR' },
            ]}
          />
        }
      >
        {error && <Alert type="error" showIcon title={error} />}
        {!list ? (
          <Skeleton active paragraph={{ rows: 8 }} />
        ) : list.length === 0 ? (
          <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="没有日志" />
        ) : (
          <div className="log-list">
            {list.map((e, i) => (
              <div key={i} className="log-line">
                <span className="log-time">{e.time && !e.time.startsWith('0001') ? new Date(e.time).toLocaleString() : ''}</span>
                <Tag color={levelColor[e.level] ?? 'default'}>{e.level}</Tag>
                <span className="log-msg">{e.message}</span>
              </div>
            ))}
          </div>
        )}
        <p className="hint">最新的在最上面。订阅地址里的令牌、密码等已脱敏；诊断包同样脱敏，可以直接发给别人排查。</p>
      </Card>
    </div>
  )
}
