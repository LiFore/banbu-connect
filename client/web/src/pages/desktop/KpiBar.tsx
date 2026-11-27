import type { ReactNode } from 'react'
export interface Kpi {
  label: string
  value: ReactNode
  title?: string // 悬停时显示的完整信息
}

export default function KpiBar({ items }: { items: Kpi[] }) {
  return (
    <div className="kpi-bar">
      {items.map((k) => (
        <div className="kpi-item" key={k.label} title={k.title}>
          <div className={`kpi-value selectable${typeof k.value === 'string' ? ' kpi-value-text' : ''}`}>{k.value}</div>
          <div className="kpi-label">{k.label}</div>
        </div>
      ))}
    </div>
  )
}
