// 与后端 boxctl/internal/desktop（Banbu Connect Lite）保持一致的类型与请求封装。

export interface ApiError {
  code: string
  message: string
  detail?: unknown
}

export interface SessionInfo {
  authenticated: boolean
  desktop?: boolean
  lite?: boolean
}

// ---- 桌面客户端状态，与 internal/desktop 的 Summary 对应 ----

// 待外壳弹出的系统通知（续期提醒、更新）。
export interface DesktopNotice {
  id: number
  kind: string
  title: string
  body: string
}

export interface DesktopSummary {
  version: string
  server: ServerStatus
  notices?: DesktopNotice[]
}

// 后台事件（更新失败、续期提醒等），新的在前。
export interface DesktopEvent {
  time: string
  level: string
  message: string
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
  last_sync?: string
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

export class ApiRequestError extends Error {
  readonly status: number
  readonly body: ApiError

  constructor(status: number, body: ApiError) {
    super(body.message)
    this.status = status
    this.body = body
  }
}

// 令牌失效（401）时广播，由 App 显示「无法访问」。
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
    status: () => request<DesktopSummary>('/desktop/status'),
    events: () => request<DesktopEvent[]>('/desktop/events'),
  },

  server: {
    status: () => request<ServerStatus>('/desktop/server'),
    bind: (b: ServerBinding) => request<ServerStatus>('/desktop/server/bind', { method: 'POST', body: JSON.stringify(b) }),
    unbind: () => request<ServerStatus>('/desktop/server/unbind', { method: 'POST' }),
    sync: () => request<ServerStatus>('/desktop/server/sync', { method: 'POST' }),
    enrollState: () => request<EnrollState>('/desktop/server/enroll'),
    enroll: (b: { url: string; name: string; fingerprint?: string }) =>
      request<EnrollState>('/desktop/server/enroll', { method: 'POST', body: JSON.stringify(b) }),
    refreshEnrollment: () => request<EnrollState>('/desktop/server/enroll/refresh', { method: 'POST' }),
    cancelEnrollment: () => request<EnrollState>('/desktop/server/enroll', { method: 'DELETE' }),
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
  // 多开：账号页，与 internal/desktop/workspaces.go 对应。
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
    openBrowser: (accountId: number) => request<unknown>(`/desktop/gemini/accounts/${accountId}/browser`, { method: 'POST' }),
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
}
