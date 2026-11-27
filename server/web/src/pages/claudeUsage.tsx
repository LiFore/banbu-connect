// Claude Code 用量的展示，客户端「Claude 用量」页和管理服务「客户端」页共用，数据与 internal/otel 的 Summary 对应。
import { Card, Segmented, Table } from 'antd'
import { BarChartOutlined } from '@ant-design/icons'
import type { OTelSummary } from '../api'
import KpiBar from './server/KpiBar'

export const usd = (n: number) => `$${n.toFixed(n >= 100 ? 0 : 2)}`
export const compact = (n: number) =>
  n >= 1e9 ? `${(n / 1e9).toFixed(1)}B` : n >= 1e6 ? `${(n / 1e6).toFixed(1)}M` : n >= 1e3 ? `${(n / 1e3).toFixed(1)}K` : `${Math.round(n)}`
export const msTime = (ms: number) => new Date(ms).toLocaleString()
const hours = (s: number) => (s >= 3600 ? `${(s / 3600).toFixed(1)} 小时` : `${Math.round(s / 60)} 分钟`)

export function DaysPicker({ value, onChange }: { value: number; onChange: (d: number) => void }) {
  return (
    <Segmented<number>
      value={value}
      onChange={onChange}
      options={[
        { label: '今天', value: 1 },
        { label: '7 天', value: 7 },
        { label: '30 天', value: 30 },
      ]}
    />
  )
}

export function UsageView({ summary }: { summary: OTelSummary }) {
  const t = summary.totals
  const tokens = t.tokens.input + t.tokens.output + t.tokens.cache_read + t.tokens.cache_creation
  return (
    <>
      <KpiBar
        items={[
          { label: '费用（估算）', value: usd(t.cost_usd), title: 'Claude Code 按 API 价格估算，订阅用户不实际计费' },
          {
            label: 'Tokens',
            value: compact(tokens),
            title: `输入 ${t.tokens.input}，输出 ${t.tokens.output}，缓存读 ${t.tokens.cache_read}，缓存写 ${t.tokens.cache_creation}`,
          },
          { label: '请求', value: t.requests },
          { label: '会话', value: t.sessions },
          { label: '提问', value: t.prompts, title: '只统计次数，不收集提示词原文' },
          { label: '工具调用', value: t.tool_calls, title: `失败 ${t.tool_failures} 次` },
          { label: '代码行', value: `+${compact(t.lines_added)} / -${compact(t.lines_removed)}` },
          { label: '活跃时长', value: hours(t.active_seconds) },
        ]}
      />
      <Card
        title={
          <span className="card-title">
            <BarChartOutlined />
            按天
          </span>
        }
      >
        <Table
          size="small"
          rowKey="day"
          pagination={false}
          dataSource={[...summary.days].reverse()}
          columns={[
            { title: '日期', dataIndex: 'day' },
            { title: '费用', dataIndex: 'cost_usd', render: usd },
            { title: 'Tokens', dataIndex: 'tokens', render: compact },
            { title: '请求', dataIndex: 'requests' },
          ]}
        />
      </Card>
      {(summary.accounts?.length ?? 0) > 1 && (
        <Card title={<span className="card-title">按账号</span>}>
          <Table
            size="small"
            rowKey="email"
            pagination={false}
            dataSource={summary.accounts}
            columns={[
              { title: '账号', dataIndex: 'email', render: (v: string) => <span className="selectable">{v || '未知（未登录 Claude 账号）'}</span> },
              { title: '会话', dataIndex: 'sessions' },
              { title: '请求', dataIndex: 'requests' },
              { title: '费用', dataIndex: 'cost_usd', render: usd },
              { title: 'Tokens', dataIndex: 'tokens', render: compact },
            ]}
          />
        </Card>
      )}
      <Card title={<span className="card-title">按模型</span>}>
        <Table
          size="small"
          rowKey="model"
          pagination={false}
          locale={{ emptyText: '还没有数据' }}
          dataSource={summary.models}
          columns={[
            { title: '模型', dataIndex: 'model' },
            { title: '请求', dataIndex: 'requests' },
            { title: '费用', dataIndex: 'cost_usd', render: usd },
            { title: 'Tokens', dataIndex: 'tokens', render: compact },
            {
              title: '耗时 P50 / P95',
              key: 'p',
              render: (_, m) => `${(m.p50_ms / 1000).toFixed(1)}s / ${(m.p95_ms / 1000).toFixed(1)}s`,
            },
          ]}
        />
      </Card>
      <Card title={<span className="card-title">最近会话</span>}>
        <Table
          size="small"
          rowKey="session_id"
          pagination={false}
          locale={{ emptyText: '还没有数据' }}
          dataSource={summary.sessions}
          columns={[
            {
              title: '工作目录',
              dataIndex: 'cwd',
              render: (v: string, s) => (
                <span className="selectable" title={s.session_id}>
                  {v || s.session_id}
                </span>
              ),
            },
            { title: '最近活动', dataIndex: 'last_ms', render: msTime },
            { title: '费用', dataIndex: 'cost_usd', render: usd },
            { title: '提问', dataIndex: 'prompts' },
            { title: '工具调用', dataIndex: 'tool_calls' },
          ]}
        />
      </Card>
    </>
  )
}
