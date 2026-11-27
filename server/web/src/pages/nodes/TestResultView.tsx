import { Alert, Descriptions } from 'antd'
import type { TestResult } from '../../api'

interface Props {
  ok: boolean
  kind: 'delay' | 'exit'
  res?: TestResult
  msg?: string
}

// 展示延迟 / 出口检测结果（M3-07/08）。
export default function TestResultView({ ok, kind, res, msg }: Props) {
  const chain = res && res.chain.length > 1 ? `链路：${res.chain.join(' → ')}` : undefined
  if (!ok) {
    return <Alert type="error" showIcon title={kind === 'delay' ? '延迟测试失败' : '出口检测失败'} description={msg} />
  }
  if (kind === 'delay') {
    return <Alert type="success" showIcon title={`延迟 ${res?.delay_ms} ms`} description={chain} />
  }
  const e = res?.exit
  return (
    <Alert
      type="success"
      showIcon
      title={`出口 IP ${e?.ip}`}
      description={
        <>
          <Descriptions column={2} size="small">
            <Descriptions.Item label="位置">{[e?.country, e?.region, e?.city].filter(Boolean).join(' / ')}</Descriptions.Item>
            <Descriptions.Item label="ASN">{e?.asn}</Descriptions.Item>
            <Descriptions.Item label="运营商" span={2}>
              {e?.org}
            </Descriptions.Item>
          </Descriptions>
          {chain}
        </>
      }
    />
  )
}
