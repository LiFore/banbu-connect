import { useCallback, useEffect, useState } from 'react'
import { Alert, App, Button, Card, Select, Skeleton, Tag, Tooltip } from 'antd'
import { LockOutlined, ReloadOutlined, ThunderboltOutlined } from '@ant-design/icons'
import { Link } from 'react-router-dom'
import { api, ApiRequestError, type CoreProxy, type GroupView } from '../../api'
import { PageHeader } from '../common'

// 线路页：切换每个 select 组当前使用的线路。选择由内核保存，重启后保留。
export default function Proxies() {
  const { message } = App.useApp()
  const [groups, setGroups] = useState<GroupView[]>([])
  const [proxies, setProxies] = useState<Record<string, CoreProxy>>()
  const [coreDown, setCoreDown] = useState(false)
  const [loaded, setLoaded] = useState(false)
  const [delays, setDelays] = useState<Record<string, { text: string; color?: string }>>({})

  const load = useCallback(
    () =>
      Promise.all([api.groups.list(), api.desktop.proxies()])
        .then(([g, p]) => {
          setGroups(g)
          setProxies(p.proxies)
          setCoreDown(false)
        })
        .catch((e: unknown) => {
          if (e instanceof ApiRequestError && e.body.code === 'core_down') setCoreDown(true)
          else message.error((e as Error).message)
        })
        .finally(() => setLoaded(true)),
    [message],
  )

  useEffect(() => {
    void load()
  }, [load])

  const select = async (group: string, name: string) => {
    try {
      await api.desktop.select(group, name)
      await load()
    } catch (e) {
      message.error((e as Error).message)
    }
  }

  const test = async (group: string) => {
    setDelays((d) => ({ ...d, [group]: { text: '测试中' } }))
    try {
      const { delay } = await api.desktop.delay(group)
      const color = delay < 300 ? 'success' : delay < 800 ? 'warning' : 'error'
      setDelays((d) => ({ ...d, [group]: { text: `${delay} ms`, color } }))
    } catch {
      setDelays((d) => ({ ...d, [group]: { text: '超时', color: 'error' } }))
    }
  }

  if (coreDown) {
    return (
      <Alert
        type="info"
        showIcon
        title="代理没有运行"
        description={
          <>
            到 <Link to="/">首页</Link> 打开代理后，才能切换线路。
          </>
        }
      />
    )
  }

  if (!loaded) {
    return (
      <div className="route-grid">
        {Array.from({ length: 6 }, (_, i) => (
          <Card key={i}>
            <Skeleton active title={false} paragraph={{ rows: 2 }} />
          </Card>
        ))}
      </div>
    )
  }

  const selectable = groups.filter((g) => g.type === 'select')
  const automatic = groups.filter((g) => g.type !== 'select' && !g.options.hidden)
  const visible = [...selectable, ...automatic]

  return (
    <div className="desktop-stack">
      <PageHeader
        title="线路"
        count={visible.length}
        actions={
          <>
            <Button icon={<ReloadOutlined />} onClick={() => void load()}>
              刷新
            </Button>
            <Button icon={<ThunderboltOutlined />} onClick={() => visible.forEach((g) => void test(g.name))}>
              全部测速
            </Button>
          </>
        }
      />
    <div className="route-grid">
      {visible.map((g) => {
        const p = proxies?.[g.name]
        const delay = delays[g.name]
        return (
          <Card key={g.id}>
            <div className="route-head">
              <span className="route-name" title={g.name}>
                {g.name}
              </span>
              {g.strict && (
                <Tag color="green" icon={<LockOutlined />}>
                  严格
                </Tag>
              )}
              {delay && (
                <Tag className="route-delay" color={delay.color}>
                  {delay.text}
                </Tag>
              )}
            </div>
            <div className="route-control">
              {g.type === 'select' ? (
                <Select
                  aria-label={`${g.name} 的线路`}
                  value={p?.now}
                  options={(p?.all ?? []).map((n) => ({ label: n, value: n }))}
                  onChange={(name: string) => select(g.name, name)}
                  showSearch
                />
              ) : (
                <span className="route-now selectable" title={p?.now}>
                  {p?.now || '由内核自动分配'}
                </span>
              )}
              <Tooltip title="测速">
                <Button aria-label={`测试 ${g.name} 的延迟`} icon={<ThunderboltOutlined />} onClick={() => test(g.name)} />
              </Tooltip>
            </div>
          </Card>
        )
      })}
    </div>
    </div>
  )
}
