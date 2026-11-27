import { useCallback, useEffect, useState } from 'react'
import { App, Button, Card, Select, Table, Tag } from 'antd'
import { GlobalOutlined } from '@ant-design/icons'
import { api, type EgressReport } from '../../api'

// 设置里的「出口检测」：手动检测本机出口，设置允许的出口国家（留空表示不限制，只在检测失败或不同站点出口不一致时提示）。
export default function EgressCard() {
  const { message } = App.useApp()
  const [report, setReport] = useState<EgressReport>()
  const [checking, setChecking] = useState(false)
  const [allowed, setAllowed] = useState<string[]>([])

  useEffect(() => {
    api.egress.settings().then((s) => setAllowed(s.allowed_countries), () => undefined)
  }, [])

  const check = useCallback(async () => {
    setChecking(true)
    try {
      setReport(await api.egress.check('claude'))
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setChecking(false)
    }
  }, [message])

  const save = async (v: string[]) => {
    try {
      setAllowed((await api.egress.saveSettings(v)).allowed_countries)
    } catch (e) {
      message.error((e as Error).message)
    }
  }

  return (
    <Card
      title={
        <span className="card-title">
          <GlobalOutlined />
          出口检测
        </span>
      }
      extra={
        <Button loading={checking} onClick={() => void check()}>
          立即检测
        </Button>
      }
    >
      <p className="hint">登录、启动账号前会自动检测出口，异常时需要确认后才能「强制上号」。应用每次打开时也会检测一次。</p>
      <div style={{ marginBottom: 12 }}>
        <div style={{ marginBottom: 4 }}>允许的出口国家（两位代码，如 US、JP；留空不限制）</div>
        <Select
          mode="tags"
          style={{ width: '100%' }}
          value={allowed}
          tokenSeparators={[',', ' ']}
          placeholder="例如 US"
          onChange={(v) => void save(v)}
        />
      </div>
      {report && (
        <>
          {report.problems.map((p) => (
            <Tag key={p} color="red" style={{ marginBottom: 4, whiteSpace: 'normal' }}>
              {p}
            </Tag>
          ))}
          {!report.abnormal && <Tag color="green">出口正常</Tag>}
          <Table
            size="small"
            pagination={false}
            rowKey="site"
            dataSource={report.results}
            columns={[
              { title: '站点', dataIndex: 'site' },
              { title: 'IP', dataIndex: 'ip', render: (v: string | undefined, r) => (r.ok ? <span className="selectable">{v}</span> : <Tag color="red">{r.error}</Tag>) },
              { title: '国家', dataIndex: 'country' },
              { title: '接入点', dataIndex: 'colo' },
              { title: '耗时', dataIndex: 'latency_ms', render: (v: number) => `${v} ms` },
            ]}
          />
        </>
      )}
    </Card>
  )
}
