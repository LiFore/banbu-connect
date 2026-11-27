import { useCallback, useEffect, useState } from 'react'
import { Alert, App, Button, Space } from 'antd'
import { Link } from 'react-router-dom'
import { api, type OTelExisting, type OTelStatus } from '../../api'

const sources: Record<OTelExisting['source'], string> = {
  settings: 'Claude Code 用户配置',
  managed: '企业托管配置（优先级最高，不能覆盖）',
  env: '环境变量',
}

export function ExistingList({ existing }: { existing: OTelExisting[] }) {
  return (
    <ul className="selectable">
      {existing.map((e) => (
        <li key={`${e.source}-${e.key}`}>
          <code>{e.key}</code> = <code>{e.value}</code>（{sources[e.source] ?? e.source}）
        </li>
      ))}
    </ul>
  )
}

// 首页提示：这台电脑上已经配置过 Claude Code 的 OpenTelemetry，所以没有自动开启用量统计。
// 用户选择开启（覆盖已有配置，关闭时恢复）或保持关闭，选过之后不再提示。
export default function OTelNotice() {
  const { message } = App.useApp()
  const [status, setStatus] = useState<OTelStatus>()
  const [saving, setSaving] = useState(false)

  const load = useCallback(() => api.otel.status().then(setStatus, () => undefined), [])
  useEffect(() => {
    void load()
    const timer = setInterval(load, 15000)
    return () => clearInterval(timer)
  }, [load])

  if (!status?.suppressed) return null

  const decide = async (enabled: boolean) => {
    setSaving(true)
    try {
      setStatus(await api.otel.update({ enabled }))
      message.success(enabled ? '已开启，之后新开的 Claude Code 会话生效' : '已保持关闭，可在「高级 · 管理服务」页底部的「数据接收设置」里随时开启')
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <Alert
      type="info"
      showIcon
      title="检测到这台电脑上已经配置过 Claude Code 的 OpenTelemetry，没有自动开启 Claude 用量统计"
      description={
        <>
          <ExistingList existing={status.claude?.existing ?? []} />
          <p className="hint">
            开启后，Claude Code 的用量数据改为发到本客户端（不含提示词和助手回复原文），并上报到管理服务。settings.json
            里的同名配置会被覆盖，原值保留，关闭时恢复。之后可在 <Link to="/advanced/server">高级 · 管理服务</Link> 页底部的「数据接收设置」里修改。
          </p>
          <Space>
            <Button type="primary" size="small" loading={saving} onClick={() => void decide(true)}>
              开启
            </Button>
            <Button size="small" disabled={saving} onClick={() => void decide(false)}>
              保持关闭
            </Button>
          </Space>
        </>
      }
    />
  )
}
