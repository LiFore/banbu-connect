import { useState } from 'react'
import { Progress, Tooltip } from 'antd'
import type { AIQuota, GeminiQuota } from '../../api'

// AI 账号的用量（账号卡片、用量页共用）：Claude / Codex 是各窗口的已用比例，Antigravity 是各窗口
// （Claude / Gemini 的 5 小时、每周，和 cockpit-tools 一致）的剩余比例；拿不到窗口时退回逐模型额度，可能有十几个：按紧张程度排序，默认只显示前几行、高度固定，其余折叠，点「展开」再看，
// 不让用量把账号信息挤下去。悬停一行看恢复时间。

export type UsageRow = { key: string; label: string; percent: number; text: string; bad: boolean; tip?: string; severity: number }

const timeText = (v?: string) => (v ? new Date(v).toLocaleString([], { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : '')

export const aiRows = (q?: AIQuota): UsageRow[] | undefined =>
  q?.windows.map((w) => ({
    key: w.key,
    label: w.label,
    percent: Math.round(w.used_percent),
    text: `已用 ${Math.round(w.used_percent)}%`,
    bad: w.used_percent >= 80,
    tip: w.reset_at ? `${timeText(w.reset_at)} 恢复` : undefined,
    severity: w.used_percent,
  }))

export const geminiRows = (q?: GeminiQuota): UsageRow[] | undefined =>
  q?.buckets.map((b) => ({
    key: b.key,
    label: b.label,
    percent: Math.round(b.remaining * 100),
    text: `剩 ${Math.round(b.remaining * 100)}%`,
    bad: b.remaining < 0.2,
    tip: [b.label, b.reset_at ? `${timeText(b.reset_at)} 恢复` : ''].filter(Boolean).join('，'),
    severity: 100 - b.remaining * 100,
  }))

export default function UsageList({
  rows,
  error,
  extra,
  empty,
  limit = 3,
}: {
  rows?: UsageRow[]
  error?: string
  extra?: string
  empty: string
  limit?: number
}) {
  const [open, setOpen] = useState(false)
  if (error) return <div className="account-usage account-usage-msg hint">用量：{error}</div>
  if (!rows || rows.length === 0) return <div className="account-usage account-usage-msg hint">{empty}</div>
  // 最紧张的排前面；Claude / Codex 的窗口只有两三个，保持原顺序。
  const sorted = rows.length > limit ? [...rows].sort((a, b) => b.severity - a.severity) : rows
  const shown = open ? sorted : sorted.slice(0, limit)
  const hidden = sorted.length - shown.length
  const badHidden = open ? 0 : sorted.slice(limit).filter((r) => r.bad).length
  return (
    <div className={open ? 'account-usage account-usage-open' : 'account-usage'}>
      {shown.map((r) => (
        <Tooltip key={r.key} title={r.tip} placement="topLeft">
          <div className="account-usage-row">
            <span className="account-usage-label">{r.label}</span>
            <Progress percent={r.percent} size="small" showInfo={false} status={r.bad ? 'exception' : 'normal'} />
            <span className="account-usage-text">{r.text}</span>
          </div>
        </Tooltip>
      ))}
      {(hidden > 0 || open || extra) && (
        <div className="account-usage-foot">
          <span className="hint">{extra}</span>
          {sorted.length > limit && (
            <button type="button" className="link-button" onClick={() => setOpen(!open)}>
              {open ? '收起' : `展开其余 ${hidden} 项${badHidden ? `（${badHidden} 项不足）` : ''}`}
            </button>
          )}
        </div>
      )}
    </div>
  )
}
