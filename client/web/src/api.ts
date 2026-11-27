// 与后端 internal/desktop、internal/panel 保持一致的类型与请求封装。


export interface ApiError {
  code: string
  message: string
  detail?: unknown
}

export interface SessionInfo {
  authenticated: boolean
  must_change: boolean
  desktop?: boolean
}

// ---- 桌面客户端，与 internal/desktop、internal/killswitch 对应 ----

export type ProxyMode = 'rule' | 'global' | 'direct'

export interface CoreStatus {
  running: boolean
  desired: boolean
  mode: ProxyMode
  tun: boolean
  tun_active: boolean
  tun_error: string
  mixed_port: number
  node_count: number
  last_error: string
  warnings: string[]
  applied_at: string
  applying: boolean
  // 路由盒：正在运行的配置摘要和应用后健康确认结果（M8-06）
  config_hash?: string
  confirm?: ConfirmState
  confirm_reason?: string
}

export type ConfirmState = 'pending' | 'confirmed' | 'unconfirmed' | 'rolled_back' | 'failed'

export type KillSwitchState = 'off' | 'protecting' | 'proxy_only' | 'blocking' | 'direct' | 'held' | 'error'

// 受保护的服务当前走的路：经本机内核、受信网络直连、阻断。与 internal/killswitch/path.go 对应。
export type KillSwitchPath = 'proxy' | 'direct' | 'hold'

export interface KillSwitchEvent {
  time: string
  level: 'info' | 'warn' | 'error'
  message: string
}

export interface RuleCheck {
  service: string
  suffix: string
  status: 'ok' | 'missing' | 'direct' | 'unknown'
  detail: string
}

export interface KillSwitchStatus {
  state: KillSwitchState
  enabled: boolean
  verdict: { path: KillSwitchPath; reason: string }
  // delegated：受信网络（路由盒）上 DNS 交给路由盒，不拦截
  dns_guard: { enabled: boolean; active: boolean; delegated?: boolean; error?: string }
  health: {
    dns_ok: boolean
    dns_detail: string
    domestic_dns_ok?: boolean
    domestic_dns_detail?: string
    tun_ok: boolean
    interface: string
  }
  rules: { installed: boolean; in_sync: boolean; diffs: string[] | null; warnings: string[] | null }
  desired: { suffixes: string[] | null; cidrs: string[] | null; interfaces: string[] | null }
  last_error: string
  rule_checks: RuleCheck[] | null
  events: KillSwitchEvent[] | null
}

// 信任 Bypass（boxctl/internal/desktop/bypass.go）：打开后账号操作不检查保护状态。
// 托盘面板和精简页的实时流量（internal/desktop/traystats.go）。速率是字节/秒。
export interface TrayStats {
  running: boolean
  up: number
  down: number
  upload_total: number
  download_total: number
  connections: number
  proxied: number
}

export interface TrustBypassStatus {
  enabled: boolean
  since?: string
}

export interface DesktopSummary {
  core: CoreStatus
  killswitch: KillSwitchStatus
  network: NetworkStatus
  strict: StrictStatus
  server: ServerStatus
  version: string
  trust_bypass?: TrustBypassStatus
}

// ---- Claude 账号登录，与 internal/dist/claude_client.go 对应 ----

export interface ClaudeAccountBrief {
  id: number
  kind?: 'claude' | 'codex' // 旧版管理服务没有这个字段，都是 claude
  email: string
  note: string
}

export type ClaudeLoginPhase = 'launching' | 'waiting_mail' | 'mail_ready' | 'opened' | 'done' | 'failed'

// 账号 Gmail 里 Anthropic 发来的邮件，与 internal/dist/claude_mail.go 对应。正文为纯文本。
export interface ClaudeMail {
  id: string
  from: string
  subject: string
  date: string
  text: string
  links: string[] // 正文里的 Claude 登录链接
  codes?: string[] // Codex 账号：OpenAI 登录验证码
}

export interface ClaudeLoginState {
  id: string
  account_id: number
  email: string
  phase: ClaudeLoginPhase
  error?: string
  started_at: string
  claimed: boolean
}

// ---- 多开：账号与工作区，与 internal/desktop/workspaces.go、aiquota.go 对应 ----

export type AIKind = 'claude' | 'codex'

export interface Workspace {
  kind: AIKind
  email: string
  dir: string
  logged_in: boolean
  login_email?: string // 工作区里实际登录的账号
  default: boolean // 已设为默认（凭据在默认目录）
  running: number // 使用这个工作区的进程数；-1 表示无法得知
  error?: string
}

export interface DefaultDir {
  email?: string
  owner?: string // 设为默认的账号；空表示用户自己登录的
  running: number
  logged_in: boolean
}

export interface AIQuotaWindow {
  key: string
  label: string
  used_percent: number
  reset_at?: string
}

export interface AIQuota {
  windows: AIQuotaWindow[]
  plan?: string
  error?: string
  checked_at: string
}

export interface AccountView {
  id: number
  kind: AIKind
  email: string
  note: string
  workspace?: Workspace
  quota?: AIQuota
  mail: boolean // 管理服务授权了这个账号的 Gmail（Claude 一键登录、看登录邮件）
}

// launch：一键启动，打开工作区的终端并直接运行 claude / codex。
export type WorkspaceActionKind = 'login' | 'app_login' | 'terminal' | 'launch' | 'set_default' | 'delete' | 'adopt_default' | 'desktop_app'

export interface WorkspaceAction {
  id: string
  action: WorkspaceActionKind
  kind: AIKind
  email: string
  cwd?: string
  status: 'pending' | 'running' | 'done' | 'failed'
  error?: string
  created_at: string
}

export interface AccountsView {
  bound: boolean
  error?: string
  shell: boolean // 托盘程序报过工作区状态
  accounts: AccountView[]
  defaults: Partial<Record<AIKind, DefaultDir>>
  default_quota: Partial<Record<AIKind, AIQuota>>
  cli_dir: string
  actions: WorkspaceAction[]
  protection: Partial<Record<AIKind, string>> // 不满足时的原因
  quota_paused: Partial<Record<AIKind, string>> // 保护服务没有放通时暂停查询额度的原因
}

// ---- 代理连接统计上报，与 internal/dist/conns.go、internal/core/connstats.go 对应 ----

export interface ConnMinute {
  minute: string
  path: string[] // 代理组 → … → 实际出站节点
  host: string
  rule: string
  payload: string
  process?: string
  source?: string
  upload: number
  download: number
  conns: number
}

export interface ConnRecord {
  start: string
  end: string
  network: string
  host: string
  destination: string
  rule: string
  payload: string
  path: string[]
  process?: string
  source?: string
  upload: number
  download: number
}

export interface ConnBatch {
  seq: number
  minutes: ConnMinute[]
  unattributed?: { minute: string; upload: number; download: number }[]
  records: ConnRecord[]
  dropped?: number
  sources?: { ip: string; mac?: string; name?: string }[]
  lan?: { ip: string; mac: string }[]
}

export interface ConnStatsStatus {
  enabled: boolean
  bound: boolean
  pending: number // 排队的批数（约等于分钟数）
  last_upload?: string
  last_error: string
  preview?: ConnBatch
}

// ---- Gemini（Antigravity）账号，与 internal/dist/gemini_client.go 对应 ----

export interface GeminiQuotaBucket {
  key: string
  label: string
  remaining: number // 0~1
  reset_at?: string
}

export interface GeminiQuota {
  buckets: GeminiQuotaBucket[]
  error?: string
  sampled_at: string
}

export interface GeminiAccountLocal {
  id: number
  email: string
  note: string
  has_credential: boolean
  version: number
  quota?: GeminiQuota
}

export interface GeminiApplied {
  account_id: number
  version: number
  error?: string
  at: string
}

export type GeminiLoginPhase = 'opening' | 'authorizing' | 'exchanging' | 'done' | 'failed'

export interface GeminiLogin {
  id: string
  account_id: number
  email: string
  phase: GeminiLoginPhase
  error?: string
  group?: string // 授权流量经过的严格组
  started_at: string
}

export interface GeminiStatus {
  bound: boolean
  synced: boolean
  last_sync?: string
  last_error: string
  accounts: GeminiAccountLocal[]
  active_account_id: number
  order: number[] // 管理服务建议的使用顺序
  applied?: GeminiApplied
  login?: GeminiLogin
  rotated?: string // 最近一次因额度将尽自动切换
  quota_paused?: string // 保护服务没有放通时暂停查询额度的原因
  launch?: GeminiLaunch // 最近一次一键启动
  // 等待中的切换（自动轮换、管理员下发）：Antigravity 在运行时，宽限期结束才关闭应用切过去
  pending?: { account_id: number; until: string; reason: string }
}

export interface GeminiLaunch {
  id: string
  account_id: number
  email: string
  at: string
  done: boolean // 托盘程序已执行
  error?: string
}

// ---- 管理服务绑定，与 internal/dist 的 Client 对应 ----

export interface ServerStatus {
  bound: boolean
  url: string
  pinned: boolean
  version: number
  published_at?: string
  note: string
  nodes: number
  groups: number
  rules: number
  last_sync?: string
  last_error: string
}

// ---- 路由盒运行观测（M11）与局域网设备，与 internal/box 对应 ----

// 本机诊断上报，与 internal/desktop/diag.go 对应。
export interface DiagStatus {
  enabled: boolean
  bound: boolean
  pending: number
  records: DiagRecord[]
  snapshot: DiagSnapshot
  last_upload?: string
  last_error: string
}

// 应用更新（首页），与 internal/desktop/appupdate.go 对应。
export interface AppUpdateProgress {
  id: string
  phase: 'requested' | 'checking' | 'downloading' | 'installing' | 'installed' | 'failed' | 'none'
  version?: string
  downloaded: number
  total: number
  error?: string
  updated_at: string
}

export interface AppUpdateStatus {
  current: string
  bound: boolean
  // mode：管理服务设的更新方式（S1.10），force 表示必须更新（管理员发布为强制更新，或低于最低版本）；旧版管理服务没有，按 normal。
  available: { version: string; notes: string; pub_date: string; size: number; mode?: 'normal' | 'push' | 'silent' | 'force'; min_version?: string } | null
  checked_at?: string
  check_error?: string
  progress: AppUpdateProgress | null
  auto?: boolean // 这次安装是按静默或强制更新自动发起的
}

// Claude Code 遥测（OpenTelemetry），与 internal/otel 对应。
export interface OTelConfig {
  enabled: boolean
  decided: boolean // 用户明确开关过
  port: number
  retention_days: number
}

export interface OTelExisting {
  key: string
  value: string
  source: 'settings' | 'managed' | 'env'
}

export interface OTelStatus {
  config: OTelConfig
  // 实际生效的开关：开启且没有因为这台电脑上已有 OTel 配置而暂不开启。
  active: boolean
  // 用户还没选择过，且已有 OTel 配置：没有自动开启，首页提示。
  suppressed: boolean
  force: boolean
  listening: boolean
  endpoint: string
  error?: string
  env: Record<string, string>
  manual_env: Record<string, string> // 手动配置时写进 settings.json env 的内容
  counts: { spans: number; logs: number; metrics: number; sessions: number; last_received_ms?: number }
  // 托盘外壳回报的 Claude Code settings.json 配置结果；外壳还没回报时没有。
  claude?: {
    configured: boolean
    settings_path: string
    existing: OTelExisting[]
    error?: string
    at: string
  }
  upload: { bound: boolean; pending: number; last_at?: string; last_error?: string }
}

export interface OTelSummary {
  since_ms: number
  until_ms: number
  totals: {
    cost_usd: number
    requests: number
    tokens: { input: number; output: number; cache_read: number; cache_creation: number }
    sessions: number
    prompts: number
    tool_calls: number
    tool_failures: number
    api_errors: number
    lines_added: number
    lines_removed: number
    commits: number
    active_seconds: number
  }
  models: { model: string; requests: number; cost_usd: number; tokens: number; p50_ms: number; p95_ms: number }[]
  // 按 Claude 账号（多开时区分各工作区）；旧版服务没有。
  accounts?: { email: string; requests: number; cost_usd: number; tokens: number; sessions: number }[]
  days: { day: string; cost_usd: number; tokens: number; requests: number }[]
  sessions: {
    session_id: string
    cwd: string
    first_ms: number
    last_ms: number
    cost_usd: number
    prompts: number
    tool_calls: number
  }[]
}

// 接入申请，与 internal/dist/enroll.go 对应。
export interface EnrollState {
  default_url: string
  default_name: string
  enrollment: {
    url: string
    name: string
    status: 'pending' | 'approved' | 'claimed' | 'rejected'
    requested_at: string
    last_error?: string
  } | null
  bound?: boolean // 刷新授权后是否已绑定
  server?: ServerStatus
}

export interface ServerBinding {
  url: string
  token: string
  fingerprint: string
}

// ---- 受信网络，与 internal/trust 对应 ----

export interface Egress {
  ok: boolean
  ip: string
  country: string
  colo?: string
  source: string
  error?: string
  checked_at: string
  latency_ms?: number
}

// 闸门直连的逐连接确认，与 internal/trust/gatecheck.go 对应。
export interface GateGroup {
  group: string
  url: string
  egress: Egress
  allowed: boolean
  reason?: string
  direct: number
  proxied: number
}

export interface GateStatus {
  enabled: boolean
  active: boolean
  groups: GateGroup[]
}

// 出口检测，与 internal/trust/scan.go 对应。
export interface ScanSite {
  name: string
  url: string
  protected: boolean
}

export interface ScanPath {
  key: string
  label: string
  hint?: string
}

export interface ScanReport {
  sites: ScanSite[]
  paths: ScanPath[]
  results: { site: string; path: string; egress: Egress }[]
  checked_at: string
}

// 严格线路的出口检查，与 internal/trust/strict.go 对应。
export interface StrictGroup {
  group: string
  egress: Egress
  allowed: boolean
  reason?: string
}

export interface StrictStatus {
  required: boolean
  allowed: string[]
  groups: StrictGroup[]
  reason?: string
}

export interface NetworkStatus {
  network: { interface: string; wifi: boolean; ssid: string; hint?: string }
  trusted: boolean
  enabled: boolean
  probing: boolean
  egress: Egress
  baseline: string
  gate?: GateStatus
  box?: BoxStatus
}

// 路由盒自动发现（ADR-073），与 internal/trust/boxnet.go 对应：管理服务下发了口令时 enabled。
export interface BoxStatus {
  enabled: boolean
  id?: string
  addr?: string
  error?: string // 验证失败的原因；只是没找到时为空
  checked_at: string
  // 排查用的详情（「高级 → 路由盒发现」）
  name: string // 查询的域名
  port: number // 挑战应答端口
  interface: string // 最近一次发现用的网卡
  dns: string[] // 最近一次问的 DNS 服务器
  trace: BoxTrace
  fails: number // 连续失败次数
  max_fails: number
  busy: boolean
  next_at?: string
}

export interface BoxTrace {
  lookups: { server: string; name: string; addrs?: string[]; ignored?: string[]; error?: string }[] | null
  attempts: { addr: string; ok: boolean; error?: string }[] | null
}

// 线路预览：指定域名命中的规则、出站链、出口和延迟，与 internal/desktop/routetest.go 对应。
export interface RouteTest {
  target: string
  url: string
  host: string
  ok: boolean
  error?: string
  status?: number
  connect_ms?: number
  latency_ms?: number
  samples?: number[]
  rule?: string
  rule_payload?: string
  path?: string[]
  group?: string
  egress?: Egress
  checked_at: string
}

export interface ServiceRouteTest extends RouteTest {
  name: string
}

// 一键刷新规则，与 internal/core/rules_refresh.go 对应。
export interface RulesRefresh {
  bound: boolean
  before_version: number
  after_version: number
  profile_error?: string
  core_running: boolean
  rule_sets: { name: string; before: number; after: number; error?: string }[]
  updated: boolean
  checked_at: string
}

export interface NetworkConfig {
  trusted_ssids: string[]
  expected_egress: string[]
  allowed_countries: string[]
  probe_urls: string[]
  baselines: Record<string, string>
  auto_start_proxy: boolean
  gate_direct: boolean
}

export interface DesktopSettings {
  mode: ProxyMode
  tun: boolean
  running: boolean
  mixed_port: number
  custom_rules: string[]
  dns_guard: boolean
  onboarded: boolean // 走完首次启动的引导屏
  mirror: string // 规则集、geo 数据的下载镜像（与 jsDelivr 路径兼容），空表示不用
  allow_lan?: boolean // 局域网设备可手动填代理端口（只有路由盒有）
}

export interface ProtectedService {
  name: string
  enabled: boolean
  group: string
  suffixes: string[]
  cidrs: string[]
}

export interface KillSwitchConfig {
  enabled: boolean
  services: ProtectedService[]
  dns_listen: string
  health?: StrictHealth // 只有路由盒有
}

// 严格线路检查的阈值（路由盒），与 internal/trust 的 StrictHealth 对应。
export interface StrictHealth {
  interval: number
  blocked_interval: number
  failures: number
  recover: number
}

export interface CoreProxy {
  type: string
  now?: string
  all?: string[]
}

// ---- 订阅（M2），与 internal/sub、internal/subscriptions 对应 ----

export interface SubOptions {
  filter_patterns: string[]
  keep_unmatched: boolean
  name_template: string
  timeout_sec: number
  retries: number
  update_hours: number // 自动更新间隔（小时），0 为不自动更新
}

export interface UserInfo {
  upload: number
  download: number
  total: number
  expire: number
}

export type FetchVia = 'direct' | 'proxy'

export interface Subscription {
  id: string
  name: string
  display_name: string
  url?: string
  has_url: boolean
  user_agent: string
  fetch_via: FetchVia
  enabled: boolean
  options: SubOptions
  userinfo: UserInfo | null
  node_count: number
  last_fetch_at: string
  last_error: string
  created_at: string
  updated_at: string
}

export interface SubscriptionInput {
  name: string
  display_name: string
  url: string
  user_agent: string
  fetch_via: FetchVia
  enabled: boolean
  options: SubOptions
}

export interface SubDefaults {
  options: SubOptions
  user_agent: string
  regions: string[]
}

export interface NodeRef {
  id: string
  display_name: string
  protocol: string
  region: string
}

export interface Diff {
  added: NodeRef[]
  removed: NodeRef[]
  changed: (NodeRef & { fields: string[] })[]
  unchanged: number
}

export interface Impact {
  node_id: string
  node_name: string
  kind: 'group' | 'node' | 'rule'
  ref_id: string
  ref_name: string
}

export interface Preview {
  preview_id: string
  expires_at: string
  format: 'clash' | 'uri'
  report: {
    parsed: number
    protocols: Record<string, number>
    filtered: string[]
    unmatched: string[]
    invalid: { name: string; error: string }[]
    duplicates: string[]
    kept: number
    regions: Record<string, number>
  }
  userinfo: UserInfo | null
  diff: Diff
  conflicts: (NodeRef & { owner_subscription: string })[]
  impacts: Impact[]
}

// ---- 节点（M3），与 internal/nodes 对应 ----

export type NodeKind = 'subscription' | 'manual' | 'isp'

export interface ProxyNode {
  id: string
  kind: NodeKind
  subscription_id: string
  subscription_name: string
  protocol: string
  display_name: string
  region: string
  server: string
  port: number
  enabled: boolean
  tags: string[]
  dialer_ref: string
  dialer_name: string
  updated_at: string
}

export type Mapping = Record<string, unknown>

export interface NodeDetail extends ProxyNode {
  mapping: Mapping
}

export interface NodeInput {
  kind: 'manual' | 'isp'
  display_name: string
  region: string
  mapping: Mapping
  dialer_ref: string
  tags: string[]
  enabled?: boolean
}

export interface NodeAttrs {
  enabled?: boolean
  tags?: string[]
  dialer_ref?: string
}

export interface SchemaField {
  key: string
  label: string
  type: 'string' | 'secret' | 'int' | 'bool' | 'select' | 'list'
  required?: boolean
  options?: string[]
  default?: unknown
  advanced?: boolean
  help?: string
}

export interface ProtocolSchema {
  type: string
  label: string
  fields: SchemaField[]
}

export interface ImportCandidate {
  kind: 'manual' | 'isp'
  display_name: string
  region: string
  protocol: string
  summary: string
  mapping?: Mapping
  warning?: string
  error?: string
}

export interface ExitInfo {
  ip: string
  country: string
  region: string
  city: string
  asn: string
  org: string
}

export interface TestResult {
  delay_ms?: number
  exit?: ExitInfo
  chain: string[]
}

export interface TestRequest {
  kind: 'delay' | 'exit'
  id?: string
  mapping?: Mapping
  dialer_ref?: string
}

export const SECRET_MASK = '******'

// ---- 代理组（M4），与 internal/groups 对应 ----

export type GroupType = 'select' | 'url-test' | 'fallback' | 'load-balance'

export interface GroupFilter {
  enabled: boolean
  regions: string[]
  subscriptions: string[]
  protocols: string[]
  kinds: NodeKind[]
  tags: string[]
  exclude_tags: string[]
  name_regex: string
}

export interface GroupOptions {
  url?: string
  interval?: number
  tolerance?: number
  timeout?: number
  max_failed_times?: number
  lazy?: boolean
  strategy?: string
  hidden?: boolean
  empty_fallback?: string
}

export interface ProxyGroup {
  id: string
  name: string
  type: GroupType
  strict: boolean
  sort_order: number
  members: { explicit: string[]; filter: GroupFilter }
  options: GroupOptions
  icon: string
}

export interface MemberRef {
  id: string
  name: string
  kind: 'node' | 'group' | 'builtin'
}

export interface GroupView extends ProxyGroup {
  resolved: MemberRef[]
  used_fallback: boolean
  warnings: string[]
}

export type GroupTemplate = 'region_auto' | 'region_manual' | 'subscription_auto'

export const emptyFilter = (): GroupFilter => ({
  enabled: false,
  regions: [],
  subscriptions: [],
  protocols: [],
  kinds: [],
  tags: [],
  exclude_tags: [],
  name_regex: '',
})

export class ApiRequestError extends Error {
  readonly status: number
  readonly body: ApiError

  constructor(status: number, body: ApiError) {
    super(body.message)
    this.status = status
    this.body = body
  }
}

// 会话失效（401）时广播，由 App 切回登录页。
export const UNAUTHENTICATED_EVENT = 'boxctl:unauthenticated'

// 桌面客户端打开窗口时把令牌放在 URL 片段里（片段不会发给服务器），读取后立刻从地址栏移除。
let desktopToken = ''
try {
  desktopToken = sessionStorage.getItem('boxctl_token') ?? ''
} catch {
  // 存储不可用时只在内存里保留
}
// 外壳打开窗口时带上 #token=…&shell=mac|win；shell 表示系统标题栏已去掉，页面要自绘。
export let desktopShell: '' | 'mac' | 'win' = ''
try {
  desktopShell = (sessionStorage.getItem('boxctl_shell') ?? '') as typeof desktopShell
} catch {
  // 存储不可用时只在内存里保留
}
const tokenMatch = location.hash.match(/^#token=([0-9a-f]+)(?:&shell=(mac|win))?$/)
if (tokenMatch) {
  desktopToken = tokenMatch[1]
  desktopShell = (tokenMatch[2] ?? '') as typeof desktopShell
  try {
    sessionStorage.setItem('boxctl_token', desktopToken)
    sessionStorage.setItem('boxctl_shell', desktopShell)
  } catch {
    // 同上
  }
  // 只去掉 #token，保留查询参数（托盘「退出」带 ?quit=1 打开精简页）。
  history.replaceState(null, '', location.pathname + location.search)
}

export async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  // 上传表单时由浏览器设置 multipart 的 Content-Type。
  const headers: Record<string, string> = init.body instanceof FormData ? {} : { 'Content-Type': 'application/json' }
  if (desktopToken) headers.Authorization = `Bearer ${desktopToken}`

  const res = await fetch(`/api/v1${path}`, {
    ...init,
    credentials: 'same-origin',
    headers: { ...headers, ...(init.headers as Record<string, string>) },
  })
  if (res.status === 204) return undefined as T
  const body = await res.json().catch(() => ({ code: 'bad_response', message: res.statusText }))
  if (!res.ok) {
    if (res.status === 401) window.dispatchEvent(new Event(UNAUTHENTICATED_EVENT))
    throw new ApiRequestError(res.status, body as ApiError)
  }
  return body as T
}

// 内核、线路、设置、管理服务的接口都在 /desktop 下。
const runtime = '/desktop'
const ksBase = '/killswitch'

// ---- 下载页（S1.11），与 internal/desktop/files.go 对应 ----

export interface RemoteFile {
  id: number
  name: string
  file_name: string
  size: number
  description: string
  pinned: boolean
  updated_at: string
  markdown: boolean
}

export interface FileDownload {
  id: string
  action: 'download'
  file_id: number
  name: string
  file_name: string
  status: 'pending' | 'running' | 'done' | 'failed' | 'canceled'
  downloaded: number
  total: number
  path?: string
  error?: string
  created_at: string
  updated_at: string
}

export const api = {
  session: () => request<SessionInfo>('/auth/session'),

  desktop: {
    status: () => request<DesktopSummary>(`${runtime}/status`),
    trayStats: () => request<TrayStats>('/desktop/tray-stats'),
    trustBypass: () => request<TrustBypassStatus>('/desktop/trust-bypass'),
    setTrustBypass: (enabled: boolean, password = '') =>
      request<TrustBypassStatus>('/desktop/trust-bypass', { method: 'PUT', body: JSON.stringify({ enabled, password }) }),
    start: () => request<CoreStatus>(`${runtime}/start`, { method: 'POST' }),
    stop: () => request<CoreStatus>(`${runtime}/stop`, { method: 'POST' }),
    settings: () => request<DesktopSettings>(`${runtime}/settings`),
    updateSettings: (patch: Partial<Omit<DesktopSettings, 'running'>>) =>
      request<DesktopSettings>(`${runtime}/settings`, { method: 'PUT', body: JSON.stringify(patch) }),
    proxies: () => request<{ proxies: Record<string, CoreProxy> }>(`${runtime}/proxies`),
    select: (group: string, name: string) =>
      request<void>(`${runtime}/proxies/${encodeURIComponent(group)}`, { method: 'PUT', body: JSON.stringify({ name }) }),
    delay: (name: string) => request<{ delay: number }>(`${runtime}/proxies/${encodeURIComponent(name)}/delay`),
    strictCheck: () => request<StrictStatus>(`${runtime}/strict/check`, { method: 'POST' }),
    // 卸载后台服务（停止并删除服务、撤销保护规则，数据保留），在服务的独立进程里进行。
    uninstall: () => request<{ ok: boolean }>('/desktop/uninstall', { method: 'POST' }),
  },

  server: {
    status: () => request<ServerStatus>(`${runtime}/server`),
    bind: (b: ServerBinding) => request<ServerStatus>(`${runtime}/server/bind`, { method: 'POST', body: JSON.stringify(b) }),
    unbind: () => request<ServerStatus>(`${runtime}/server/unbind`, { method: 'POST' }),
    sync: () => request<ServerStatus>(`${runtime}/server/sync`, { method: 'POST' }),
    enrollState: () => request<EnrollState>(`${runtime}/server/enroll`),
    enroll: (b: { url: string; name: string; fingerprint?: string }) =>
      request<EnrollState>(`${runtime}/server/enroll`, { method: 'POST', body: JSON.stringify(b) }),
    refreshEnrollment: () => request<EnrollState>(`${runtime}/server/enroll/refresh`, { method: 'POST' }),
    cancelEnrollment: () => request<EnrollState>(`${runtime}/server/enroll`, { method: 'DELETE' }),
    diagnostics: () => request<DiagStatus>('/desktop/diagnostics'),
    setDiagnostics: (enabled: boolean) =>
      request<DiagStatus>('/desktop/diagnostics', { method: 'PUT', body: JSON.stringify({ enabled }) }),
    uploadDiagnostics: () => request<DiagStatus>('/desktop/diagnostics/upload', { method: 'POST' }),
    // 代理连接统计上报（桌面客户端和路由盒都有）。
    connStats: () => request<ConnStatsStatus>(`${runtime}/server/conn-stats`),
    setConnStats: (enabled: boolean) =>
      request<ConnStatsStatus>(`${runtime}/server/conn-stats`, { method: 'PUT', body: JSON.stringify({ enabled }) }),
    uploadConnStats: () => request<ConnStatsStatus>(`${runtime}/server/conn-stats/upload`, { method: 'POST' }),
  },
  // 一键发起 Claude Code 登录，与 internal/desktop/claude.go 对应。
  claude: {
    accounts: () => request<ClaudeAccountBrief[]>('/desktop/claude/accounts'),
    current: () => request<{ login: ClaudeLoginState | null }>('/desktop/claude/login'),
    login: (accountId: number) =>
      request<{ login: ClaudeLoginState }>('/desktop/claude/login', { method: 'POST', body: JSON.stringify({ account_id: accountId }) }),
    mails: (accountId: number) => request<ClaudeMail[]>(`/desktop/claude/accounts/${accountId}/mails`),
    // 由托盘外壳在默认浏览器打开（几秒内）；只接受 Claude 登录链接。
    openLink: (url: string) =>
      request<{ login: ClaudeLoginState | null }>('/desktop/claude/open-link', { method: 'POST', body: JSON.stringify({ url }) }),
  },
  // 多开：账号页，与 internal/desktop/workspaces.go 对应。保护状态不满足时返回 409（code: protection，fix 是去处理的页面）。
  accounts: {
    view: () => request<AccountsView>('/desktop/accounts'),
    act: (a: { action: WorkspaceActionKind; kind: AIKind; email?: string; cwd?: string }) =>
      request<WorkspaceAction>('/desktop/workspaces/actions', { method: 'POST', body: JSON.stringify(a) }),
  },
  // Gemini（Antigravity）账号，与 internal/desktop/gemini.go 对应。
  gemini: {
    status: () => request<GeminiStatus>('/desktop/gemini'),
    setActive: (accountId: number) =>
      request<GeminiStatus>('/desktop/gemini/active', { method: 'POST', body: JSON.stringify({ account_id: accountId }) }),
    // 由托盘外壳用这个账号的固定浏览器档案打开 Google 账号页（几秒内）。
    openBrowser: (accountId: number) => request<{ group: string }>(`/desktop/gemini/accounts/${accountId}/browser`, { method: 'POST' }),
    // 一键启动：切到这个账号，托盘程序写好凭据后启动 Antigravity 应用。
    launch: (accountId: number) => request<GeminiStatus>(`/desktop/gemini/accounts/${accountId}/launch`, { method: 'POST' }),
    // 等待中的切换不等宽限期，马上关闭 Antigravity 切过去。
    applyNow: () => request<GeminiStatus>('/desktop/gemini/apply-now', { method: 'POST' }),
    // 立即重新查一次额度，几秒后出现在 status 里。
    refreshQuota: () => request<{ ok: boolean }>('/desktop/gemini/quota/refresh', { method: 'POST' }),
    login: (accountId: number) =>
      request<{ login: GeminiLogin }>('/desktop/gemini/login', { method: 'POST', body: JSON.stringify({ account_id: accountId }) }),
    // 回跳打不开时粘贴浏览器地址栏里的回跳地址。
    finishLogin: (id: string, url: string) =>
      request<GeminiStatus>(`/desktop/gemini/login/${id}/callback`, { method: 'POST', body: JSON.stringify({ url }) }),
  },
  otel: {
    status: () => request<OTelStatus>('/desktop/otel/status'),
    // 带 enabled 即算用户明确选择过。
    update: (c: Partial<Omit<OTelConfig, 'decided'>>) =>
      request<OTelStatus>('/desktop/otel/config', { method: 'PUT', body: JSON.stringify(c) }),
    upload: () => request<OTelStatus>('/desktop/otel/upload', { method: 'POST' }),
    // 让托盘程序立即核对并写入 Claude Code 配置（3 秒内），结果在 status.claude 里。
    syncClaude: () => request<OTelStatus>('/desktop/otel/claude/sync', { method: 'POST' }),
    summary: (days: number) => request<OTelSummary>(`/desktop/otel/summary?days=${days}`),
  },
  files: {
    list: () => request<RemoteFile[]>('/desktop/files'),
    content: (id: number) => request<{ content: string }>(`/desktop/files/${id}/content`),
    download: (id: number) => request<FileDownload>(`/desktop/files/${id}/download`, { method: 'POST' }),
    downloads: () => request<FileDownload[]>('/desktop/files/downloads'),
    cancel: (id: string) => request<FileDownload>(`/desktop/files/downloads/${id}/cancel`, { method: 'POST' }),
    remove: (id: string) => request<{ ok: boolean }>(`/desktop/files/downloads/${id}`, { method: 'DELETE' }),
    reveal: (id: string) => request<unknown>(`/desktop/files/downloads/${id}/reveal`, { method: 'POST' }),
    open: (id: string) => request<unknown>(`/desktop/files/downloads/${id}/open`, { method: 'POST' }),
  },
  appUpdate: {
    status: (refresh = false) => request<AppUpdateStatus>(`/desktop/app-update${refresh ? '?refresh=1' : ''}`),
    install: () => request<AppUpdateStatus>('/desktop/app-update/install', { method: 'POST' }),
    restart: () => request<AppUpdateStatus>('/desktop/app-update/restart', { method: 'POST' }),
  },
  network: {
    config: () => request<NetworkConfig>('/desktop/network/config'),
    update: (c: NetworkConfig) => request<NetworkConfig>('/desktop/network/config', { method: 'PUT', body: JSON.stringify(c) }),
    trustCurrent: () => request<NetworkStatus>('/desktop/network/trust-current', { method: 'POST' }),
    acceptEgress: () => request<NetworkStatus>('/desktop/network/accept-egress', { method: 'POST' }),
    recheck: () => request<NetworkStatus>('/desktop/network/recheck', { method: 'POST' }),
    boxRecheck: () => request<NetworkStatus>('/desktop/network/box-recheck', { method: 'POST' }),
    scan: () => request<ScanReport>('/desktop/egress/scan', { method: 'POST' }),
    services: () => request<ServiceRouteTest[]>('/desktop/egress/services', { method: 'POST' }),
    testRoute: (target: string) => request<RouteTest>('/desktop/route/test', { method: 'POST', body: JSON.stringify({ target }) }),
    refreshRules: () => request<RulesRefresh>('/desktop/rules/refresh', { method: 'POST' }),
  },
  killswitch: {
    config: () => request<KillSwitchConfig>(`${ksBase}/config`),
    update: (c: Omit<KillSwitchConfig, 'enabled'>) =>
      request<KillSwitchConfig>(`${ksBase}/config`, { method: 'PUT', body: JSON.stringify(c) }),
    enable: (force: boolean) =>
      request<KillSwitchStatus>(`${ksBase}/enable`, { method: 'POST', body: JSON.stringify({ force }) }),
    disable: () => request<KillSwitchStatus>(`${ksBase}/disable`, { method: 'POST' }),
  },

  subscriptions: {
    list: () => request<Subscription[]>('/subscriptions'),
    defaults: () => request<SubDefaults>('/subscriptions/defaults'),
    get: (id: string, reveal = false) => request<Subscription>(`/subscriptions/${id}${reveal ? '?reveal=1' : ''}`),
    create: (input: SubscriptionInput) =>
      request<Subscription>('/subscriptions', { method: 'POST', body: JSON.stringify(input) }),
    update: (id: string, input: SubscriptionInput) =>
      request<Subscription>(`/subscriptions/${id}`, { method: 'PUT', body: JSON.stringify(input) }),
    remove: (id: string, confirm = false) =>
      request<void>(`/subscriptions/${id}${confirm ? '?confirm=1' : ''}`, { method: 'DELETE' }),
    preview: (id: string) => request<Preview>(`/subscriptions/${id}/preview`, { method: 'POST' }),
    apply: (id: string, previewId: string, confirm: boolean) =>
      request<Diff>(`/subscriptions/${id}/apply`, {
        method: 'POST',
        body: JSON.stringify({ preview_id: previewId, confirm }),
      }),
  },

  nodes: {
    list: () => request<ProxyNode[]>('/nodes'),
    schemas: () => request<ProtocolSchema[]>('/nodes/schemas'),
    get: (id: string, reveal = false) => request<NodeDetail>(`/nodes/${id}${reveal ? '?reveal=1' : ''}`),
    create: (input: NodeInput) => request<NodeDetail>('/nodes', { method: 'POST', body: JSON.stringify(input) }),
    update: (id: string, input: NodeInput) =>
      request<NodeDetail>(`/nodes/${id}`, { method: 'PUT', body: JSON.stringify(input) }),
    patch: (id: string, attrs: NodeAttrs) =>
      request<ProxyNode>(`/nodes/${id}`, { method: 'PATCH', body: JSON.stringify(attrs) }),
    remove: (id: string, confirm = false) =>
      request<void>(`/nodes/${id}${confirm ? '?confirm=1' : ''}`, { method: 'DELETE' }),
    batchEnable: (ids: string[], enabled: boolean) =>
      request<{ updated: number }>('/nodes/batch', { method: 'POST', body: JSON.stringify({ ids, enabled }) }),
    // 订阅节点跳过（skipped）；被引用时返回 references_exist，确认后带 confirm 重试。
    batchRemove: (ids: string[], confirm = false) =>
      request<{ deleted: number; skipped: number }>('/nodes/batch-delete', { method: 'POST', body: JSON.stringify({ ids, confirm }) }),
    import: (text: string) =>
      request<ImportCandidate[]>('/nodes/import', { method: 'POST', body: JSON.stringify({ text }) }),
    test: (req: TestRequest) => request<TestResult>('/nodes/test', { method: 'POST', body: JSON.stringify(req) }),
  },

  groups: {
    list: () => request<GroupView[]>('/groups'),
    create: (g: Omit<ProxyGroup, 'id' | 'sort_order'>) =>
      request<GroupView>('/groups', { method: 'POST', body: JSON.stringify(g) }),
    update: (id: string, g: Omit<ProxyGroup, 'id' | 'sort_order'>) =>
      request<GroupView>(`/groups/${id}`, { method: 'PUT', body: JSON.stringify(g) }),
    remove: (id: string, confirm = false) =>
      request<void>(`/groups/${id}${confirm ? '?confirm=1' : ''}`, { method: 'DELETE' }),
    resolve: (g: Omit<ProxyGroup, 'id' | 'sort_order'> & { id?: string }) =>
      request<GroupView>('/groups/resolve', { method: 'POST', body: JSON.stringify(g) }),
    template: (template: GroupTemplate, regions: string[], excludeISP: boolean) =>
      request<{ created: string[]; skipped: string[] }>('/groups/templates', {
        method: 'POST',
        body: JSON.stringify({ template, regions, exclude_isp: excludeISP }),
      }),
    reorder: (ids: string[]) => request<void>('/groups/order', { method: 'POST', body: JSON.stringify({ ids }) }),
  },
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
