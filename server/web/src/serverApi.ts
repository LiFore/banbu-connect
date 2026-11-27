// 管理服务（boxctl server）的接口，与 boxctl/internal/dist/server_routes.go 对应。
import { request, type ConnRecord, type OTelSummary } from './api'

export interface GroupSummary {
  name: string
  type: 'select' | 'url-test' | 'fallback' | 'load-balance'
  strict: boolean
  members: string[]
}

export interface ProfileSummary {
  version: number
  published_at?: string
  note: string
  nodes: string[]
  groups: GroupSummary[]
  selections: Record<string, string>
  rules: number
  rule_sets: number // -1：没有下发，客户端用内置默认
  // 方案（ADR-062、ADR-064）
  variants: { name: string; version: string; devices: number; nodes: number; groups: number; rules: number; rule_sets: number }[]
  profile_version: string // 「默认」方案的版本号
}

// 远程规则集与 GEOIP（内置规则），与 internal/gen/rulesets.go 对应。顺序即规则顺序。
export interface RuleSet {
  type?: 'rule-set' | 'geoip'
  name: string
  behavior?: 'domain' | 'ipcidr' | 'classical'
  format?: '' | 'yaml' | 'text' | 'mrs'
  url?: string
  interval?: number
  target: string
  no_resolve?: boolean
  disabled?: boolean
}

export interface RuleSimulation {
  match: { index: number; rule: string; source: string; target: string; final?: string } | null
  uncertain: { index: number; rule: string; reason: string }[]
}

// 规则集里一条的类别（与 internal/dist/rulesets.go 的 classifyRule 对应）。
export type RuleKind = 'domain' | 'suffix' | 'keyword' | 'regex' | 'ip' | 'other'

export interface RuleSetPreview {
  url: string
  count: number
  kinds: Partial<Record<RuleKind, number>>
  matched: number
  items: { rule: string; kind: RuleKind; value: string }[]
  bytes: number
  fetched_at: string
  cached: boolean
  warning?: string
}

export interface RuleSetPreviewQuery {
  url: string
  format?: string
  behavior?: string
  refresh?: boolean
  q?: string
  kind?: '' | 'domains' | RuleKind
  offset?: number
  limit?: number
}

// 规则集统计：条数、各类别条数、前几个域名；missing 表示还没缓存。
export interface RuleSetStat {
  url: string
  format: string
  count: number
  kinds: Partial<Record<RuleKind, number>>
  sample: string[]
  fetched_at?: string
  cached: boolean
  missing?: boolean
  error?: string
}

// 方案的内容（ADR-064）。nodes、groups 为 null 表示全部；rule_sets 为 null 表示客户端用内置默认。
export interface ProfileContent {
  nodes: string[] | null
  groups: string[] | null
  selections: Record<string, string>
  rules: string[] // 带分段标记
  rule_sets: RuleSet[] | null
}

// 方案（ADR-062、ADR-064），与 internal/dist/rule_profiles.go 对应。「默认」也是一个方案（default）。
export interface RuleProfile extends ProfileContent {
  id: number
  name: string
  code: string
  version: string // 代码-时间戳
  default: boolean
  devices: number[]
  created_at: string
  updated_at: string
}

export interface ProfileVersion {
  version: string
  note: string
  created_at: string
  current: boolean
  content: ProfileContent
  nodes: number // -1：全部
  groups: number
  rules: number
  rule_sets: number // -1：内置默认
}

export interface Draft {
  draft: ProfileSummary
  current: ProfileSummary | null
  changed: boolean
}

export interface ProfileMeta {
  version: number
  note: string
  created_at: string
  devices: number
}

export interface DistSettings {
  selections: Record<string, string>
  rules: string[]
  strict_egress: string[]
  box_secret: string // 路由盒发现口令（ADR-073），空表示不启用自动发现
}

export interface Device {
  id: number
  name: string
  created_at: string
  last_seen: string
  last_ip: string
  app_version: string
  platform: string
  os_version: string // 客户端上报的系统版本
  profile_version: number
  revoked: boolean
  diag_at: string // 最近一次上报诊断数据
  reported_name: string // 客户端最近一次上报的电脑名
  name_custom: boolean // 名称是管理员改的，不再被上报的电脑名覆盖
  user_id: number // 所属用户（User.id），0 表示不属于任何用户
}

// 用户：有姓名，名下可以有多台设备；账号可以分配给用户（名下所有设备都能用），与 internal/dist/users.go 对应。
export interface User {
  id: number
  name: string
  note: string
  devices: number[] // 名下的设备（不含已吊销）
  created_at: string
}

export interface UserInput {
  name: string
  note: string
  devices?: number[] // 不传表示不改名下的设备
}

// 客户端接入状态，与 internal/dist/access.go 对应。
export type AccessMode = 'box' | 'trusted_wifi' | 'local' | 'hold' | 'off'

export interface AccessState {
  mode: AccessMode
  reason?: string
  core_running: boolean
  tun: boolean
  proxy_mode?: string
  killswitch?: string
  gate: boolean
  box_id?: string
  box_addr?: string
  network: { interface: string; wifi: boolean; trusted: boolean; ssid_readable: boolean; hint?: string }
  since: string // 客户端时钟
}

export interface DeviceAccess {
  device_id: number
  state: AccessState
  since: string // 管理服务记的这一段的开始时间
  reported_at: string
  online: boolean
}

export interface AccessSession {
  mode: AccessMode
  box_id?: string
  start_at: string
  end_at: string // 空表示还在进行
  last_seen: string
}

// 客户端上报的 Claude Code 用量，与 internal/dist/otel.go 对应。
export interface DeviceOTelUsage {
  device_id: number
  cost_usd: number
  requests: number
  tokens: number
  sessions: number
  last_ms: number
}

// 诊断数据，与 internal/dist/diagnostics.go 对应。不含 Wi-Fi 名称、访问记录、开关机等使用情况。
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

// 接入申请，与 internal/dist/enroll.go 对应。
export interface Enrollment {
  id: number
  name: string
  platform: string
  os_version: string
  app_version: string
  ip: string
  status: 'pending' | 'approved' | 'claimed' | 'rejected'
  device_id: number | null
  created_at: string
  updated_at: string
}

export interface DeviceDiagnostics {
  snapshot: DiagSnapshot | null
  snapshot_at: string
  records: (DiagRecord & { id: number; received_at: string })[]
}

export interface Release {
  id: number
  version: string
  target: string
  notes: string
  file_name: string
  signature: string
  size: number
  published: boolean
  created_at: string
  devices: number[] // 灰度名单：只有这些客户端能收到；空表示全部
  mode: UpdateMode // 更新方式（S1.10）
  min_version: string // 低于它的客户端必须更新
  storage: StorageKind // 文件存在哪里（S1.11）
}

// ---- 对象存储与云盘（S1.11），与 internal/dist/storage.go、files.go 对应 ----

export type StorageKind = 'local' | 'oss'

export interface StorageConfig {
  enabled: boolean
  region: string
  endpoint: string
  bucket: string
  access_key_id: string
  access_key_secret?: string // 只用于保存，留空表示不改
  has_secret: boolean
  prefix: string
  active: StorageKind // 新文件存到哪里
  local_releases: number
  local_files: number
}

export interface StorageTest {
  ok: boolean
  error?: string
  cors_ready: boolean
  cors_error?: string
}

export interface DriveFile {
  id: number
  name: string
  file_name: string
  storage: StorageKind
  size: number
  description: string
  pinned: boolean
  sort: number
  published: boolean
  created_at: string
  updated_at: string
  markdown: boolean
}

export interface SignedURL {
  url: string
  headers?: Record<string, string>
}

export interface UploadPlan {
  id: string
  storage: StorageKind
  part_size: number
  parts: SignedURL[]
}

// ---- 消息推送与用量提醒（S1.12），与 internal/dist/notify.go、alerts.go 对应 ----

export interface NotifyConfig {
  bark_enabled: boolean
  bark_server: string
  bark_group: string
  bark_keys_masked: string[]
  feishu_enabled: boolean
  feishu_hook_set: boolean
  feishu_secret_set: boolean
  panel_url: string
}

// 保存时敏感项不传（undefined）表示不改。
export interface NotifyInput {
  bark_enabled: boolean
  bark_server: string
  bark_group: string
  bark_keys?: string[]
  feishu_enabled: boolean
  feishu_hook?: string
  feishu_secret?: string
  panel_url: string
}

export interface TrafficRule {
  id?: string
  name: string
  period: 'day' | 'month'
  limit_gb: number
  enabled: boolean
}

export interface AlertRules {
  timezone: string
  traffic: TrafficRule[]
  sub_used_percent: number
  sub_remain_gb: number
  sub_expire_days: number
  ai_used_percent: number
  ai_expire_days: number
  sub_update: boolean
}

export interface TrafficTarget {
  name: string
  kind: 'node' | 'group'
  day: number
  month: number
}

export interface NotifyRecord {
  id: number
  kind: string
  title: string
  body: string
  result: string
  created_at: string
}

export type DriveFileEdit = Partial<Pick<DriveFile, 'name' | 'file_name' | 'description' | 'pinned' | 'published'>>

export type UpdateMode = 'normal' | 'push' | 'silent' | 'force'

// 一台客户端的版本与最近一次更新进度，与 internal/dist/update_policy.go 对应。
export interface DeviceUpdate {
  device_id: number
  name: string
  platform: string
  app_version: string
  last_seen: string
  version: string
  mode: string
  phase: string
  error: string
  updated_at: string
}

export interface ServerInfo {
  self_signed: boolean
  plain_http: boolean
  fingerprint: string
}

export interface ReleaseUpload {
  version: string
  target: string
  notes: string
  signature: string
  file: File
}

export type AccountKind = 'claude' | 'codex'
export type AccountType = 'personal' | 'team' // 只对 Claude 有效
export type TeamRole = 'owner' | 'member'

export interface ClaudeAccount {
  id: number
  kind: AccountKind
  email: string
  note: string
  devices: number[] // 生效的客户端：直接分配的加上分配的用户名下的；空表示没有客户端能用（ADR-060）
  direct_devices: number[] // 直接分配的客户端
  users: number[] // 分配给的用户
  gmail_authorized: boolean
  // 自建邮箱（IMAP，如 BillionMail）：登录名为空表示用账号邮箱；密码只返回有没有。填了密码时优先于 Gmail。
  mail_user: string
  has_mailbox: boolean
  mail_source: '' | 'imap' | 'gmail' // 读登录邮件的方式
  // 管理员标记，只用于显示和筛选。
  plan: string
  account_type: AccountType
  team_role: '' | TeamRole
  team_name: string
  team_domain: string // 空表示按账号邮箱的域名
  last_login_at: string
  last_login_device: string
  last_error: string
  // 托管的 OAuth 凭据（ADR-061）：只返回有没有、版本和来源。
  has_credential: boolean
  cred_version: number
  cred_updated_at: string
  cred_source: string
  quota?: AIQuota // 客户端最近上报的额度
  quota_device: string
  sub_expires: string // 订阅到期日 YYYY-MM-DD，管理员填写，只用于提醒
  auth_test?: AuthTest
  // 令牌集中续期（ADR-071）：Refresh Token 只在管理服务（Codex 另有持有者客户端），客户端只拿 Access Token。
  holder_device: number // Codex：指定的持有者，0 表示自动
  holder: number // Codex：实际的持有者
  cred_expires: number // 托管的 Access Token 过期时间（Unix 毫秒），0 表示不知道
  refresh_at: string
  refresh_error: string
  refresh?: AIRefreshState
  created_at: string
  updated_at: string
}

// 进行中或最近失败的续期，与 internal/dist/ai_refresh.go 对应。
export interface AIRefreshState {
  id: string
  phase: 'notice' | 'running' | 'waiting' | 'failed'
  device_id?: number
  device_name?: string
  message?: string
  deadline?: string
  permanent?: boolean
  manual?: boolean
}

// Claude / Codex 额度，与 internal/dist/ai_cred.go 对应。
export interface AIQuota {
  windows: { key: string; label: string; used_percent: number; reset_at?: string }[]
  plan?: string
  error?: string
  checked_at: string
}

// 鉴权测试：由一台分配到的在线客户端执行（ADR-061）。
export interface AuthTest {
  id: string
  kind: AccountKind | 'gemini'
  account_id: number
  device_id: number
  device_name: string
  status: 'pending' | 'ok' | 'failed'
  message?: string
  quota?: AIQuota
  started_at: string
}

// 新建时可以带上凭据（两栏各填一个令牌，或粘贴凭据 JSON）；修改时类型不能变，凭据另走 setClaudeCredential。
export interface ClaudeAccountInput {
  kind: AccountKind
  email: string
  note: string
  devices: number[] // 直接分配的客户端
  users?: number[] // 分配给的用户
  sub_expires: string
  holder_device?: number // 只对 Codex 有效，0 表示自动
  plan?: string
  account_type?: AccountType
  team_role?: '' | TeamRole
  team_name?: string
  team_domain?: string
  mail_user?: string
  mail_password?: string // 修改时为空表示不变
  access_token?: string
  refresh_token?: string
}

// ---- 遥测（代理流量统计），与 internal/dist/conns_query.go 对应 ----

export type TelemetryRangeName = '1h' | '24h' | '7d' | '30d'

// ---- AI 账号在线时长（按切换时间算）----

export type OnlineKind = 'claude' | 'codex' | 'gemini'

export interface AIOnlineTotal {
  device_id: number
  device_name: string
  kind: OnlineKind
  account_id: number
  email: string
  seconds: number
  sessions: number
  online_since: string // 此刻在线时这一段的开始时间
  last_at: string // 最近一次在线（进行中为空）
}

export interface AIOnlineDay {
  day: string
  kind: OnlineKind
  account_id: number
  seconds: number
}

export interface AIOnlineSession {
  id: number
  device_id: number
  device_name: string
  kind: OnlineKind
  account_id: number
  email: string
  start_at: string
  end_at: string // 空表示还在线
  seconds: number
}

export interface AIOnlineStats {
  from: string
  to: string
  totals: AIOnlineTotal[]
  days: AIOnlineDay[]
  sessions: AIOnlineSession[]
}

export interface AIOnlineSummary {
  kind: OnlineKind
  account_id: number
  today: number
  week: number
  devices: { device_id: number; device_name: string; today: number; week: number; online_since: string }[]
}

export interface AIOnlineQuery {
  from: string
  to: string
  device?: number
  kind?: OnlineKind
  account?: number
}

export interface Traffic {
  upload: number
  download: number
  conns: number
}

export interface TrendPoint extends Traffic {
  time: string
  unattributed: number
}

// key 用于筛选：客户端键、域名、「规则类型,内容」、节点名、代理组名、程序名。
export interface RankItem extends Traffic {
  key: string
  label: string
}

export interface TelemetryFilter {
  client?: string
  host?: string
  rule?: string
  node?: string
  group?: string
  hop?: string
}

export interface TelemetryRange {
  name: TelemetryRangeName
  from: string
  to: string
  step: 'minute' | 'hour' | 'day'
}

export interface TelemetryOverview {
  range: TelemetryRange
  online: number
  clients: number
  active: number
  total: Traffic
  unattributed: number
  trend: TrendPoint[]
  top_clients: RankItem[]
  top_hosts: RankItem[]
  top_nodes: RankItem[]
  top_rules: RankItem[]
}

export interface TopologyNode extends Traffic {
  id: string
  name: string
  kind: 'client' | 'hop' | 'host'
  depth: number
  filter: TelemetryFilter
}

export interface TopologyLink extends Traffic {
  source: string
  target: string
}

export interface Topology {
  nodes: TopologyNode[]
  links: TopologyLink[]
}

export interface TelemetryRecord {
  id: number
  client: string
  client_label: string
  start: string
  end: string
  network: string
  host: string
  destination: string
  rule: string
  payload: string
  path: string[]
  process?: string
  upload: number
  download: number
}

export interface TelemetryDetail {
  range: TelemetryRange
  filter: TelemetryFilter
  total: Traffic
  trend: TrendPoint[]
  clients: RankItem[]
  hosts: RankItem[]
  rules: RankItem[]
  groups: RankItem[]
  nodes: RankItem[]
  processes: RankItem[]
  records: TelemetryRecord[]
  device_id?: number
}

export interface UsageSeries {
  key: string
  label: string
  total: Traffic
  points: TrendPoint[]
}

export interface ClaudeOverview {
  summary: OTelSummary
  devices: DeviceOTelUsage[]
}

export interface LiveView {
  at?: string
  conns: ConnRecord[]
}

const telemetryQuery = (range: TelemetryRangeName, f: TelemetryFilter = {}, extra: Record<string, string> = {}) =>
  new URLSearchParams({ range, ...Object.fromEntries(Object.entries(f).filter(([, v]) => v)), ...extra }).toString()

// Gemini（Antigravity）账号，与 internal/dist/gemini.go 对应；凭据只返回有没有、版本和来源。
export interface GeminiAccount {
  id: number
  email: string
  note: string
  max_devices: number
  devices: number[] // 生效的客户端：直接分配的加上分配的用户名下的
  direct_devices: number[] // 直接分配的客户端
  users: number[] // 分配给的用户
  has_credential: boolean
  cred_version: number
  cred_updated_at: string
  cred_source: string
  sub_expires: string
  plan: string
  auth_test?: AuthTest
  created_at: string
  updated_at: string
}

// Gemini 额度汇总与调度，与 internal/dist/gemini_rotate.go 对应。
export interface GeminiUsage {
  account_id: number
  buckets: { key: string; label: string; remaining: number; reset_at?: string }[]
  known: boolean
  min: number // 最紧一项的剩余比例
  min_label: string
  reset_at: string
  burn_per_hour: number
  exhaust_at: string // 预计耗尽时间；恢复前不会耗尽时为空
  sampled_at: string
}

export interface GeminiDeviceView {
  device_id: number
  name: string
  active_account_id: number
  reported_at: string
  order: number[]
  pending_switch: number
  suggestion?: { account_id: number; reason: string }
}

export interface GeminiOverview {
  usage: GeminiUsage[]
  devices: GeminiDeviceView[]
  threshold: number
}

// Antigravity 的 OAuth 客户端；密钥只返回有没有。
export interface GeminiOAuthInfo {
  client_id: string
  has_secret: boolean
}

export interface GeminiAccountInput {
  email: string
  note: string
  max_devices: number
  devices: number[] // 直接分配的客户端（受设备上限限制）
  users?: number[] // 分配给的用户：名下的设备都能用，不占设备上限
  sub_expires: string
  plan?: string
  access_token?: string
  refresh_token?: string
}

// 自建邮箱的 IMAP 服务器（如 BillionMail），与 internal/dist/imap.go 对应。
export interface MailServer {
  host: string
  port: number
  security: 'tls' | 'starttls' | 'none'
  skip_verify: boolean
  folders: string
}

// Google OAuth 客户端（桌面应用类型）；密钥只返回有没有。
export interface GmailClient {
  client_id: string
  has_secret: boolean
  redirect_uri: string
}

export const serverApi = {
  gmailClient: () => request<GmailClient>('/dist/gmail-client'),
  setGmailClient: (clientId: string, clientSecret: string) =>
    request<GmailClient>('/dist/gmail-client', {
      method: 'PUT',
      body: JSON.stringify({ client_id: clientId, client_secret: clientSecret }),
    }),
  authorizeGmail: (id: number) => request<{ url: string }>(`/dist/claude-accounts/${id}/gmail/authorize`, { method: 'POST' }),
  finishGmail: (id: number, url: string) =>
    request<ClaudeAccount>(`/dist/claude-accounts/${id}/gmail/callback`, { method: 'POST', body: JSON.stringify({ url }) }),
  mailServer: () => request<MailServer>('/dist/mail-server'),
  setMailServer: (m: MailServer) => request<MailServer>('/dist/mail-server', { method: 'PUT', body: JSON.stringify(m) }),
  clearMailbox: (id: number) => request<ClaudeAccount>(`/dist/claude-accounts/${id}/mailbox`, { method: 'DELETE' }),
  claudeAccounts: () => request<ClaudeAccount[]>('/dist/claude-accounts'),
  createClaudeAccount: (a: ClaudeAccountInput) =>
    request<ClaudeAccount>('/dist/claude-accounts', { method: 'POST', body: JSON.stringify(a) }),
  updateClaudeAccount: (id: number, a: ClaudeAccountInput) =>
    request<ClaudeAccount>(`/dist/claude-accounts/${id}`, { method: 'PUT', body: JSON.stringify(a) }),
  deleteClaudeAccount: (id: number) => request<{ ok: boolean }>(`/dist/claude-accounts/${id}`, { method: 'DELETE' }),
  setClaudeCredential: (id: number, accessToken: string, refreshToken: string) =>
    request<ClaudeAccount>(`/dist/claude-accounts/${id}/credential`, {
      method: 'PUT',
      body: JSON.stringify({ access_token: accessToken, refresh_token: refreshToken }),
    }),
  clearClaudeCredential: (id: number) => request<ClaudeAccount>(`/dist/claude-accounts/${id}/credential`, { method: 'DELETE' }),
  refreshClaude: (id: number) => request<ClaudeAccount>(`/dist/claude-accounts/${id}/refresh`, { method: 'POST' }),
  authTestClaude: (id: number) => request<AuthTest>(`/dist/claude-accounts/${id}/auth-test`, { method: 'POST' }),
  setGeminiCredential: (id: number, accessToken: string, refreshToken: string) =>
    request<GeminiAccount>(`/dist/gemini-accounts/${id}/credential`, {
      method: 'PUT',
      body: JSON.stringify({ access_token: accessToken, refresh_token: refreshToken }),
    }),
  authTestGemini: (id: number) => request<AuthTest>(`/dist/gemini-accounts/${id}/auth-test`, { method: 'POST' }),
  testClaudeAccount: (id: number) =>
    request<{ ok: boolean; error?: string }>(`/dist/claude-accounts/${id}/test`, { method: 'POST' }),
  telemetryOverview: (range: TelemetryRangeName) => request<TelemetryOverview>(`/dist/telemetry/overview?${telemetryQuery(range)}`),
  telemetryTopology: (range: TelemetryRangeName, f: TelemetryFilter = {}, limit = 12) =>
    request<Topology>(`/dist/telemetry/topology?${telemetryQuery(range, f, { limit: String(limit) })}`),
  telemetryDetail: (range: TelemetryRangeName, f: TelemetryFilter, before?: number) =>
    request<TelemetryDetail>(`/dist/telemetry/detail?${telemetryQuery(range, f, before ? { before: String(before) } : {})}`),
  telemetryUsage: (days: number, step: 'hour' | 'day') => request<UsageSeries[]>(`/dist/telemetry/usage?days=${days}&step=${step}`),
  aiOnline: (q: AIOnlineQuery) => {
    const p = new URLSearchParams({ from: q.from, to: q.to, tz: Intl.DateTimeFormat().resolvedOptions().timeZone })
    if (q.device) p.set('device', String(q.device))
    if (q.kind) p.set('kind', q.kind)
    if (q.account) p.set('account', String(q.account))
    return request<AIOnlineStats>(`/dist/ai-online?${p}`)
  },
  aiOnlineSummary: () => {
    const d = new Date()
    d.setHours(0, 0, 0, 0)
    return request<AIOnlineSummary[]>(`/dist/ai-online/summary?today=${encodeURIComponent(d.toISOString().replace(/\.\d{3}Z$/, 'Z'))}`)
  },
  telemetryClaude: (days: number) => request<ClaudeOverview>(`/dist/telemetry/claude?days=${days}`),
  deviceLive: (id: number) => request<LiveView>(`/dist/devices/${id}/live`),
  geminiOAuth: () => request<GeminiOAuthInfo>('/dist/gemini-oauth'),
  setGeminiOAuth: (clientId: string, clientSecret: string) =>
    request<GeminiOAuthInfo>('/dist/gemini-oauth', { method: 'PUT', body: JSON.stringify({ client_id: clientId, client_secret: clientSecret }) }),
  geminiAccounts: () => request<GeminiAccount[]>('/dist/gemini-accounts'),
  geminiOverview: () => request<GeminiOverview>('/dist/gemini-overview'),
  switchGemini: (deviceId: number, accountId: number) =>
    request<{ id: string }>(`/dist/gemini-devices/${deviceId}/switch`, { method: 'POST', body: JSON.stringify({ account_id: accountId }) }),
  createGeminiAccount: (a: GeminiAccountInput) =>
    request<GeminiAccount>('/dist/gemini-accounts', { method: 'POST', body: JSON.stringify(a) }),
  updateGeminiAccount: (id: number, a: GeminiAccountInput) =>
    request<GeminiAccount>(`/dist/gemini-accounts/${id}`, { method: 'PUT', body: JSON.stringify(a) }),
  deleteGeminiAccount: (id: number) => request<{ ok: boolean }>(`/dist/gemini-accounts/${id}`, { method: 'DELETE' }),
  clearGeminiCredential: (id: number) => request<GeminiAccount>(`/dist/gemini-accounts/${id}/credential`, { method: 'DELETE' }),
  info: () => request<ServerInfo>('/dist/info'),
  draft: () => request<Draft>('/dist/draft'),
  profiles: () => request<ProfileMeta[]>('/dist/profiles'),
  publish: (note: string) => request<ProfileSummary>('/dist/publish', { method: 'POST', body: JSON.stringify({ note }) }),
  settings: () => request<DistSettings>('/dist/settings'),
  updateSettings: (s: Partial<DistSettings>) => request<{ ok: boolean }>('/dist/settings', { method: 'PUT', body: JSON.stringify(s) }),
  devices: () => request<Device[]>('/dist/devices'),
  users: () => request<User[]>('/dist/users'),
  createUser: (u: UserInput) => request<User>('/dist/users', { method: 'POST', body: JSON.stringify(u) }),
  updateUser: (id: number, u: UserInput) => request<User>(`/dist/users/${id}`, { method: 'PUT', body: JSON.stringify(u) }),
  deleteUser: (id: number) => request<{ ok: boolean }>(`/dist/users/${id}`, { method: 'DELETE' }),
  setDeviceUser: (id: number, userId: number) =>
    request<Device>(`/dist/devices/${id}/user`, { method: 'PUT', body: JSON.stringify({ user_id: userId }) }),
  deviceAccess: () => request<DeviceAccess[]>('/dist/access'),
  deviceAccessHistory: (id: number, days = 7) => request<AccessSession[]>(`/dist/devices/${id}/access?days=${days}`),
  createDevice: () => request<{ device: Device; token: string }>('/dist/devices', { method: 'POST' }),
  // name 为空时恢复自动命名（用客户端上报的电脑名）
  renameDevice: (id: number, name: string) =>
    request<Device>(`/dist/devices/${id}/name`, { method: 'PUT', body: JSON.stringify({ name }) }),
  revokeDevice: (id: number) => request<{ ok: boolean }>(`/dist/devices/${id}/revoke`, { method: 'POST' }),
  deleteDevice: (id: number) => request<{ ok: boolean }>(`/dist/devices/${id}`, { method: 'DELETE' }),
  enrollments: () => request<Enrollment[]>('/dist/enrollments'),
  approveEnrollment: (id: number, name: string) =>
    request<Enrollment>(`/dist/enrollments/${id}/approve`, { method: 'POST', body: JSON.stringify({ name }) }),
  rejectEnrollment: (id: number) => request<{ ok: boolean }>(`/dist/enrollments/${id}/reject`, { method: 'POST' }),
  deleteEnrollment: (id: number) => request<{ ok: boolean }>(`/dist/enrollments/${id}`, { method: 'DELETE' }),
  diagnostics: (id: number, kind = '', before = 0) =>
    request<DeviceDiagnostics>(`/dist/devices/${id}/diagnostics?` + new URLSearchParams({ kind, before: String(before), limit: '200' })),
  ruleSets: () => request<{ rule_sets: RuleSet[]; custom: boolean }>('/dist/rule-sets'),
  ruleProfiles: () => request<RuleProfile[]>('/dist/rule-profiles'),
  createRuleProfile: (name: string, copyFrom: number) =>
    request<RuleProfile>('/dist/rule-profiles', { method: 'POST', body: JSON.stringify({ name, copy_from: copyFrom, devices: [] }) }),
  updateRuleProfile: (id: number, p: ProfileContent & { name: string; devices: number[]; note?: string }) =>
    request<RuleProfile>(`/dist/rule-profiles/${id}`, { method: 'PUT', body: JSON.stringify(p) }),
  profileVersions: (id: number) => request<ProfileVersion[]>(`/dist/rule-profiles/${id}/versions`),
  rollbackProfile: (id: number, version: string) =>
    request<RuleProfile>(`/dist/rule-profiles/${id}/rollback`, { method: 'POST', body: JSON.stringify({ version }) }),
  deleteRuleProfile: (id: number) => request<{ ok: boolean }>(`/dist/rule-profiles/${id}`, { method: 'DELETE' }),
  defaultRuleSets: () => request<RuleSet[]>('/dist/rule-sets/defaults'),
  // null 表示恢复内置默认。
  updateRuleSets: (list: RuleSet[] | null) =>
    request<{ ok: boolean }>('/dist/rule-sets', { method: 'PUT', body: JSON.stringify({ rule_sets: list }) }),
  // 规则预览（M5-06）：按已保存的草稿离线模拟。
  simulateRule: (q: { query: string; port: number; network: string; profile_id?: number }) =>
    request<RuleSimulation>('/dist/rules/simulate', { method: 'POST', body: JSON.stringify(q) }),
  // 规则集内容（ADR-064）：管理服务缓存下载结果，按类别、关键字筛选分页。
  previewRuleSet: (q: RuleSetPreviewQuery) =>
    request<RuleSetPreview>('/dist/rule-sets/preview', { method: 'POST', body: JSON.stringify(q) }),
  ruleSetStats: (ruleSets: RuleSet[], opts: { fetch?: boolean; refresh?: boolean } = {}) =>
    request<RuleSetStat[]>('/dist/rule-sets/stats', { method: 'POST', body: JSON.stringify({ rule_sets: ruleSets, ...opts }) }),
  otelDevices: (days: number) => request<DeviceOTelUsage[]>(`/dist/otel/devices?days=${days}`),
  deviceOTel: (id: number, days: number) => request<OTelSummary>(`/dist/devices/${id}/otel?days=${days}`),
  releases: () => request<{ releases: Release[]; targets: string[] }>('/dist/releases'),
  uploadRelease: (r: ReleaseUpload) => {
    // 文件必须放在表单最后：服务端边读边存，前面的字段要先读到。
    const form = new FormData()
    form.append('version', r.version)
    form.append('target', r.target)
    form.append('notes', r.notes)
    form.append('signature', r.signature)
    form.append('file', r.file, r.file.name)
    return request<Release>('/dist/releases', { method: 'POST', body: form })
  },
  publishRelease: (id: number, published: boolean) =>
    request<{ ok: boolean }>(`/dist/releases/${id}/publish`, { method: 'POST', body: JSON.stringify({ published }) }),
  setReleaseDevices: (id: number, devices: number[]) =>
    request<{ ok: boolean }>(`/dist/releases/${id}/devices`, { method: 'PUT', body: JSON.stringify({ devices }) }),
  deleteRelease: (id: number) => request<{ ok: boolean }>(`/dist/releases/${id}`, { method: 'DELETE' }),
  setReleasePolicy: (id: number, mode: UpdateMode, minVersion: string) =>
    request<{ ok: boolean }>(`/dist/releases/${id}/policy`, { method: 'PUT', body: JSON.stringify({ mode, min_version: minVersion }) }),
  updateStatus: () => request<DeviceUpdate[]>('/dist/update-status'),
  storage: () => request<StorageConfig>('/dist/storage'),
  saveStorage: (c: Partial<StorageConfig>) => request<StorageConfig>('/dist/storage', { method: 'PUT', body: JSON.stringify(c) }),
  testStorage: (c: Partial<StorageConfig>) => request<StorageTest>('/dist/storage/test', { method: 'POST', body: JSON.stringify(c) }),
  fixStorageCORS: () => request<{ ok: boolean }>('/dist/storage/cors', { method: 'POST' }),
  migrateStorage: () => request<{ releases: number; files: number; errors: string[] }>('/dist/storage/migrate', { method: 'POST' }),
  files: () => request<DriveFile[]>('/dist/files'),
  updateFile: (id: number, e: DriveFileEdit) => request<DriveFile>(`/dist/files/${id}`, { method: 'PUT', body: JSON.stringify(e) }),
  deleteFile: (id: number) => request<{ ok: boolean }>(`/dist/files/${id}`, { method: 'DELETE' }),
  reorderFiles: (ids: number[]) => request<{ ok: boolean }>('/dist/files-order', { method: 'PUT', body: JSON.stringify({ ids }) }),
  fileContent: (id: number) => request<{ content: string }>(`/dist/files/${id}/content`),
  fileDownloadURL: (id: number) => `/api/v1/dist/files/${id}/download`,
  startUpload: (fileName: string, size: number, replaceId = 0) =>
    request<UploadPlan>('/dist/file-uploads', { method: 'POST', body: JSON.stringify({ file_name: fileName, size, replace_id: replaceId }) }),
  completeUpload: (
    id: string,
    body: { parts: { number: number; etag: string }[]; name?: string; description?: string; pinned?: boolean; published?: boolean },
  ) => request<DriveFile>(`/dist/file-uploads/${id}/complete`, { method: 'POST', body: JSON.stringify(body) }),
  notify: () => request<NotifyConfig>('/dist/notify'),
  saveNotify: (c: NotifyInput) => request<NotifyConfig>('/dist/notify', { method: 'PUT', body: JSON.stringify(c) }),
  testNotify: (c: NotifyInput) => request<{ ok: boolean; error: string }>('/dist/notify/test', { method: 'POST', body: JSON.stringify(c) }),
  notifyLog: () => request<NotifyRecord[]>('/dist/notify/log'),
  alerts: () => request<AlertRules>('/dist/alerts'),
  saveAlerts: (r: AlertRules) => request<AlertRules>('/dist/alerts', { method: 'PUT', body: JSON.stringify(r) }),
  alertTargets: () => request<TrafficTarget[]>('/dist/alerts/targets'),
  checkAlerts: () => request<{ sent: number }>('/dist/alerts/check', { method: 'POST' }),
  abortUpload: (id: string) => request<{ ok: boolean }>(`/dist/file-uploads/${id}`, { method: 'DELETE' }),
}
