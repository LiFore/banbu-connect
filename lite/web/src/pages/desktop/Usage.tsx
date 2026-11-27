import { useEffect, useState, type ReactNode } from 'react'
import { Card, Empty, Skeleton } from 'antd'
import { DashboardOutlined } from '@ant-design/icons'
import { api, type AccountsView, type AIKind, type GeminiStatus } from '../../api'
import ClaudeUsage from './ClaudeUsage'
import OTelNotice from './OTelNotice'
import UsageList, { aiRows, geminiRows } from './UsageList'
import VendorLogo, { type VendorKind } from './VendorLogo'
import { PageHeader } from '../common'

// 用量页（BXC-F0-049）：AI 账号的额度（Claude、Codex、Gemini）和 Claude Code 的历史用量（按账号区分）。
// 只看 AI 账号的额度和 Claude Code 用量。
export default function Usage() {
  return (
    <div className="desktop-stack">
      <PageHeader title="用量" />
      <OTelNotice />
      <QuotaCard />
      <ClaudeUsage />
    </div>
  )
}

function QuotaCard() {
  const [accounts, setAccounts] = useState<AccountsView>()
  const [gemini, setGemini] = useState<GeminiStatus>()
  const [loaded, setLoaded] = useState(false)

  useEffect(() => {
    const load = () =>
      Promise.allSettled([api.accounts.view(), api.gemini.status()]).then(([a, g]) => {
        if (a.status === 'fulfilled') setAccounts(a.value)
        if (g.status === 'fulfilled') setGemini(g.value)
        setLoaded(true)
      })
    void load()
    const timer = setInterval(load, 30000)
    return () => clearInterval(timer)
  }, [])

  const rows: { key: string; kind: VendorKind; label: string; sub?: string; node: ReactNode }[] = []
  for (const kind of ['claude', 'codex'] as AIKind[]) {
    const def = accounts?.default_quota[kind]
    if (def)
      rows.push({
        key: `${kind}-default`,
        kind,
        label: '默认账号',
        sub: accounts?.defaults[kind]?.email,
        node: <UsageList rows={aiRows(def)} error={def.error} extra={def.plan} empty="没有数据" />,
      })
    for (const a of accounts?.accounts.filter((x) => x.kind === kind && x.quota) ?? []) {
      rows.push({ key: `${kind}-${a.id}`, kind, label: a.email, sub: a.note, node: <UsageList rows={aiRows(a.quota)} error={a.quota?.error} extra={a.quota?.plan} empty="没有数据" /> })
    }
  }
  for (const g of gemini?.accounts.filter((x) => x.quota) ?? []) {
    rows.push({ key: `gemini-${g.id}`, kind: 'gemini', label: g.email, sub: g.note, node: <UsageList rows={geminiRows(g.quota)} error={g.quota?.error} empty="没有数据" /> })
  }

  return (
    <Card
      title={
        <span className="card-title">
          <DashboardOutlined />
          额度
        </span>
      }
    >
      {!loaded ? (
        <Skeleton active title={false} paragraph={{ rows: 2 }} />
      ) : rows.length === 0 ? (
        <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有额度数据：在「账号」页登录后，几分钟内出现" />
      ) : (
        <div className="usage-grid">
          {rows.map((r) => (
            <div key={r.key} className="usage-item">
              <div className="usage-item-head">
                <VendorLogo kind={r.kind} size={24} />
                <span className="account-email selectable" title={r.label}>
                  {r.label}
                </span>
                {r.sub && <span className="hint usage-item-sub">{r.sub}</span>}
              </div>
              {r.node}
            </div>
          ))}
        </div>
      )}
    </Card>
  )
}
