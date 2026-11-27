// AI 账号在线时长的显示工具（账号卡片和遥测页共用）。

// 秒数显示成「2 小时 5 分」「35 分」「不到 1 分」。
export function formatDuration(sec: number): string {
  const m = Math.floor(sec / 60)
  if (m < 1) return sec > 0 ? '不到 1 分' : '0'
  const h = Math.floor(m / 60)
  if (h === 0) return `${m} 分`
  return m % 60 ? `${h} 小时 ${m % 60} 分` : `${h} 小时`
}

// 时间显示成「11-25 14:03」；当天的只显示时间。
export function formatClock(v: string): string {
  if (!v) return ''
  const d = new Date(v)
  const now = new Date()
  const time = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
  if (d.toDateString() === now.toDateString()) return time
  return `${d.getMonth() + 1}-${d.getDate()} ${time}`
}
