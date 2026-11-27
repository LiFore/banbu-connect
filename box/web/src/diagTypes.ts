// 诊断数据的类型，与 boxctl/internal/dist/diagnostics.go 对应。不含 Wi-Fi 名称、访问记录、开关机等使用情况。
export interface DiagRecord {
  seq: number
  time: string
  kind: string // egress、strict_egress、network、killswitch、health、core
  level: 'info' | 'warn' | 'error' | string
  message: string
  fields?: Record<string, string>
}

export interface DiagEgress {
  ok: boolean
  ip?: string
  country?: string
  error?: string
  latency_ms?: number
  checked_at: string
}

export interface DiagSnapshot {
  time: string
  killswitch: string
  path: string
  reason?: string
  dns_ok: boolean
  tun_ok: boolean
  core_running: boolean
  core_error?: string
  tun_error?: string
  network: { interface: string; wifi: boolean; trusted: boolean; ssid_readable: boolean; hint?: string }
  egress?: DiagEgress
  strict?: { group: string; allowed: boolean; egress: DiagEgress }[]
}
