import { useEffect, useMemo, useState } from 'react'
import { Alert, Card, Empty, Segmented, Skeleton, Table } from 'antd'
import { BarChartOutlined } from '@ant-design/icons'
import { serverApi, type TelemetryFilter, type UsageSeries } from '../../../serverApi'
import { formatBytes } from '../../common'
import EChart from './EChart'
import { bytesOf, usageOption } from './charts'

type Span = 'hour7' | 'day30' | 'day90'

const spans: Record<Span, { days: number; step: 'hour' | 'day'; label: string }> = {
  hour7: { days: 7, step: 'hour', label: '每小时（7 天）' },
  day30: { days: 30, step: 'day', label: '每天（30 天）' },
  day90: { days: 90, step: 'day', label: '每天（90 天）' },
}

// 流量用量（BXC-F0-053）：每个客户端（路由盒细到设备）每小时 / 每天的代理流量，长期保留；不含直连。
export default function Usage({ onDrill }: { onDrill: (f: TelemetryFilter) => void }) {
  const [span, setSpan] = useState<Span>('day30')
  // 结果带上是哪个范围的：切换范围后、新结果回来前显示骨架。
  const [res, setRes] = useState<{ span: Span; data?: UsageSeries[]; error?: string }>()
  const data = res?.span === span ? res.data : undefined
  const error = res?.span === span ? res.error : undefined

  useEffect(() => {
    let alive = true
    const s = spans[span]
    serverApi.telemetryUsage(s.days, s.step).then(
      (d) => alive && setRes({ span, data: d }),
      (e: Error) => alive && setRes({ span, error: e.message }),
    )
    return () => {
      alive = false
    }
  }, [span])

  const option = useMemo(() => (data ? usageOption(data, spans[span].step) : undefined), [data, span])
  const total = (data ?? []).reduce((n, s) => n + bytesOf(s.total), 0) || 1

  return (
    <Card
      title={
        <span className="card-title">
          <BarChartOutlined />
          代理流量用量
        </span>
      }
      extra={
        <Segmented<Span>
          size="small"
          value={span}
          onChange={setSpan}
          options={(Object.keys(spans) as Span[]).map((k) => ({ label: spans[k].label, value: k }))}
        />
      }
    >
      {error && <Alert type="error" showIcon title={error} />}
      {!data ? (
        !error && <Skeleton active paragraph={{ rows: 6 }} />
      ) : data.length === 0 ? (
        <Empty description="这段时间没有代理流量" />
      ) : (
        <>
          {option && <EChart option={option} height={280} />}
          <Table<UsageSeries>
            size="small"
            rowKey="key"
            dataSource={data}
            pagination={false}
            onRow={(s) => ({ onClick: () => onDrill({ client: s.key }), className: 'clickable-row' })}
            columns={[
              { title: '客户端', dataIndex: 'label', ellipsis: true },
              { title: '流量', key: 'bytes', width: 110, render: (_, s) => formatBytes(bytesOf(s.total)) },
              { title: '占比', key: 'pct', width: 70, render: (_, s) => `${Math.round((bytesOf(s.total) / total) * 100)}%` },
              { title: '上传', key: 'up', width: 110, render: (_, s) => formatBytes(s.total.upload) },
              { title: '下载', key: 'down', width: 110, render: (_, s) => formatBytes(s.total.download) },
              { title: '连接', key: 'conns', width: 90, render: (_, s) => s.total.conns },
            ]}
          />
        </>
      )}
    </Card>
  )
}
