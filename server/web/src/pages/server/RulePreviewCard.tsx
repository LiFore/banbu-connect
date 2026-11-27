import { useState } from 'react'
import { Alert, App, Button, Card, Input, Select, Space, Tag } from 'antd'
import { SearchOutlined } from '@ant-design/icons'
import { serverApi, type RuleSimulation } from '../../serverApi'

// 规则预览（M5-06）：输入域名或 IP，按方案已保存的内容离线模拟（ADR-064：方案的代理组、规则、规则集、线路选择），
// 返回命中的规则和最终出口组。
export default function RulePreviewCard({ profileId, draftChanged }: { profileId: number; draftChanged: boolean }) {
  const { message } = App.useApp()
  const [query, setQuery] = useState('')
  const [port, setPort] = useState('443')
  const [network, setNetwork] = useState<'tcp' | 'udp'>('tcp')
  const [result, setResult] = useState<RuleSimulation>()
  const [loading, setLoading] = useState(false)

  const run = async () => {
    if (!query.trim()) return
    setLoading(true)
    try {
      setResult(await serverApi.simulateRule({ query: query.trim(), port: Number(port) || 0, network, profile_id: profileId }))
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setLoading(false)
    }
  }

  return (
    <Card
      title={
        <span className="card-title">
          <SearchOutlined />
          规则预览
        </span>
      }
    >
      <Space wrap>
        <Input
          style={{ width: 260 }}
          placeholder="域名或 IP，如 claude.ai、8.8.8.8"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onPressEnter={run}
        />
        <Input style={{ width: 90 }} addonBefore="端口" value={port} onChange={(e) => setPort(e.target.value)} />
        <Select value={network} onChange={setNetwork} options={[{ value: 'tcp', label: 'TCP' }, { value: 'udp', label: 'UDP' }]} />
        <Button type="primary" loading={loading} onClick={run}>
          模拟
        </Button>
      </Space>
      {draftChanged && <Alert style={{ marginTop: 12 }} type="info" showIcon title="按已保存的草稿模拟；页面上还没保存的改动不算在内。" />}
      {result && (
        <div className="simulate-result">
          {result.match ? (
            <Alert
              type="success"
              showIcon
              title={
                <span>
                  命中第 {result.match.index} 条 <code>{result.match.rule}</code>（{result.match.source}）→ 出口 <Tag color="purple">{result.match.target}</Tag>
                  {result.match.final && result.match.final !== result.match.target && <span>，默认成员 {result.match.final}</span>}
                </span>
              }
            />
          ) : (
            <Alert type="warning" showIcon title="没有命中任何规则" />
          )}
          {result.uncertain.length > 0 && (
            <Alert
              style={{ marginTop: 8 }}
              type="warning"
              showIcon
              title="之前有几条规则离线无法判断，实际可能先命中它们"
              description={
                <ul className="simulate-list">
                  {result.uncertain.map((u) => (
                    <li key={u.index}>
                      第 {u.index} 条 <code>{u.rule}</code>：{u.reason}
                    </li>
                  ))}
                </ul>
              }
            />
          )}
        </div>
      )}
    </Card>
  )
}
