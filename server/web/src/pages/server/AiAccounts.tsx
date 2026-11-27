import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react'
import { Alert, App, Button, Card, Checkbox, Dropdown, Empty, Form, Input, Modal, Progress, Segmented, Select, Skeleton, Space, Tag, Tooltip } from 'antd'
import {
  ApiOutlined,
  CheckCircleFilled,
  ClockCircleOutlined,
  CloseCircleFilled,
  CloudServerOutlined,
  DeleteOutlined,
  DesktopOutlined,
  EditOutlined,
  ExclamationCircleFilled,
  FieldTimeOutlined,
  GoogleOutlined,
  KeyOutlined,
  MailOutlined,
  ReloadOutlined,
  MoreOutlined,
  PlusOutlined,
  SearchOutlined,
  SettingOutlined,
  StopOutlined,
  SyncOutlined,
  TeamOutlined,
} from '@ant-design/icons'
import {
  serverApi,
  type AccountKind,
  type AIOnlineSummary,
  type AIQuota,
  type AIRefreshState,
  type ClaudeAccount,
  type Device,
  type User,
  type GeminiAccount,
  type GeminiOAuthInfo,
  type GeminiOverview,
  type GeminiUsage,
  type GmailClient,
  type MailServer,
} from '../../serverApi'
import { AssignedList } from './assign'
import { PageHeader } from '../common'
import { AuthorizeModal, ClaudeEditModal, ClientModal, emailDomain, isGmail, MailServerModal } from './ClaudeAccounts'
import { GeminiDevicesCard, GeminiEditModal, OAuthModal } from './GeminiAccounts'
import { formatClock, formatDuration } from './aiOnline'

// 管理服务「AI 账号」（ADR-061）：Claude、Codex、Gemini 账号放在一页，三类分开添加，每个账号一张紧凑卡片：
// 头部是类型、邮箱和管理员标记（套餐、Claude 的个人号 / Team 号），中间是客户端上报的用量，底部一行状态（令牌、鉴权、
// 续期、收信、订阅、客户端），细节放在提示里，有问题的单独一行红字。上方筛选栏按类型、套餐、Team、域名、状态和关键字筛选。
// 鉴权测试和用量查询都由分配到的客户端执行，管理服务自己不连 Anthropic / OpenAI / Google。

type Kind = 'claude' | 'codex' | 'gemini'
type Item = { kind: 'claude' | 'codex'; a: ClaudeAccount } | { kind: 'gemini'; a: GeminiAccount }

const kindTag: Record<Kind, { label: string; color: string }> = {
  claude: { label: 'Claude', color: 'orange' },
  codex: { label: 'Codex', color: 'blue' },
  gemini: { label: 'Gemini', color: 'green' },
}

const short = (v?: string) =>
  v
    ? new Date(v).toLocaleString([], {
        month: 'numeric',
        day: 'numeric',
        hour: '2-digit',
        minute: '2-digit',
      })
    : ''

// 订阅还剩几天到期（按本地日期，当天到期为 0，已过期为负数）；没填时为 undefined。
const expiryDays = (date?: string) => {
  if (!date) return undefined
  const [y, m, d] = date.split('-').map(Number)
  const now = new Date()
  return Math.round((new Date(y, m - 1, d).getTime() - new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime()) / 86400000)
}
// 托管的 Access Token 什么时候过期。
const tokenExpired = (ms: number) => ms > 0 && ms < Date.now()
const tokenExpiry = (ms: number) => (ms < Date.now() ? 'Access Token 已过期' : `${short(new Date(ms).toISOString())} 过期`)

const expirySoon = 7 // 页面上提前几天标出来（推送提醒的天数在「通知」里设置）

// 已用比例对应的颜色：≥90% 红，≥70% 橙。
const usedColor = (used: number) => (used >= 90 ? 'var(--ant-color-error, #ff4d4f)' : used >= 70 ? 'var(--ant-color-warning, #f59e0b)' : undefined)

const teamRoleName = { owner: '主号', member: '成员号' } as const

// 账号的 Team 域名：没单独填时用邮箱的域名。
const teamDomain = (a: ClaudeAccount) => a.team_domain || emailDomain(a.email)

// 账号要处理的问题（筛选「需处理」和卡片底部红字用），没有问题时为空数组。
function problems(it: Item): string[] {
  const a = it.a
  const out: string[] = []
  if (a.devices.length === 0) out.push('没有分配客户端')
  if (!a.has_credential) out.push(it.kind === 'gemini' ? '还没有登录' : '没有托管令牌')
  if (a.auth_test?.status === 'failed') out.push(`鉴权失败：${a.auth_test.message || '未知原因'}`)
  if (it.kind !== 'gemini') {
    const r = it.a.refresh
    if (r?.phase === 'failed') out.push(`续期失败：${r.message ?? ''}`)
    else if (!r && it.a.refresh_error) out.push(`续期失败：${it.a.refresh_error}`)
    if (it.a.last_error) out.push(`登录失败：${it.a.last_error}`)
  }
  const left = expiryDays(a.sub_expires)
  if (left !== undefined && left < 0) out.push(`订阅已过期 ${-left} 天`)
  return out
}

// ---- 筛选 ----

type Status = '' | 'problem' | 'expiring' | 'no-token' | 'no-mail' | 'no-device'
type Filters = { kind: 'all' | Kind; q: string; plan?: string; type?: string; team?: string; domain?: string; status: Status }
const emptyFilters: Filters = { kind: 'all', q: '', status: '' }
const filterKey = 'boxctl.aiAccounts.filters'

// 筛选条件按浏览器记住（只是方便，读不到时从头开始）。
function loadFilters(): Filters {
  try {
    const v = JSON.parse(localStorage.getItem(filterKey) ?? 'null') as Partial<Filters> | null
    return v ? { ...emptyFilters, ...v } : emptyFilters
  } catch {
    return emptyFilters
  }
}

const accountType = (it: Item) => (it.kind !== 'claude' ? '' : it.a.account_type === 'team' ? `team-${it.a.team_role}` : 'personal')

function matches(it: Item, f: Filters) {
  const a = it.a
  if (f.kind !== 'all' && it.kind !== f.kind) return false
  if (f.plan !== undefined && (a.plan || '') !== f.plan) return false
  if (f.type && accountType(it) !== f.type) return false
  if (f.team && (it.kind !== 'claude' || it.a.account_type !== 'team' || it.a.team_name !== f.team)) return false
  if (f.domain && emailDomain(a.email) !== f.domain) return false
  const left = expiryDays(a.sub_expires)
  switch (f.status) {
    case 'problem':
      if (problems(it).length === 0 && !(left !== undefined && left <= expirySoon)) return false
      break
    case 'expiring':
      if (left === undefined || left > expirySoon) return false
      break
    case 'no-token':
      if (a.has_credential) return false
      break
    case 'no-mail':
      if (it.kind === 'gemini' || it.a.mail_source) return false
      break
    case 'no-device':
      if (a.devices.length > 0) return false
      break
  }
  const q = f.q.trim().toLowerCase()
  if (q) {
    const text = [a.email, a.note, a.plan, it.kind !== 'gemini' ? `${it.a.team_name} ${it.a.team_domain}` : ''].join(' ').toLowerCase()
    if (!text.includes(q)) return false
  }
  return true
}

const uniq = (v: string[]) => [...new Set(v.filter(Boolean))].sort((x, y) => x.localeCompare(y))

function FilterBar({ items, f, onChange, shown }: { items: Item[]; f: Filters; onChange: (f: Filters) => void; shown: number }) {
  const set = (p: Partial<Filters>) => onChange({ ...f, ...p })
  const count = (k: Kind) => items.filter((i) => i.kind === k).length
  const plans = uniq(items.map((i) => i.a.plan))
  const hasNoPlan = items.some((i) => !i.a.plan)
  const teams = uniq(items.map((i) => (i.kind === 'claude' && i.a.account_type === 'team' ? i.a.team_name : '')))
  const domains = uniq(items.map((i) => emailDomain(i.a.email)))
  const active = JSON.stringify({ ...f, kind: 'all' }) !== JSON.stringify({ ...emptyFilters, kind: 'all' })
  return (
    <Card size="small" className="ai-filter">
      <Segmented<Filters['kind']>
        value={f.kind}
        onChange={(kind) => set({ kind })}
        options={[
          { label: `全部 ${items.length}`, value: 'all' },
          { label: `Claude ${count('claude')}`, value: 'claude' },
          { label: `Codex ${count('codex')}`, value: 'codex' },
          { label: `Gemini ${count('gemini')}`, value: 'gemini' },
        ]}
      />
      <Input
        allowClear
        prefix={<SearchOutlined />}
        placeholder="邮箱、备注、Team"
        value={f.q}
        onChange={(e) => set({ q: e.target.value })}
        className="ai-filter-search"
      />
      <Select
        allowClear
        placeholder="套餐"
        value={f.plan}
        onChange={(plan?: string) => set({ plan })}
        options={[...plans.map((p) => ({ value: p, label: p })), ...(hasNoPlan ? [{ value: '', label: '未标记' }] : [])]}
        popupMatchSelectWidth={false}
        className="ai-filter-select"
      />
      <Select
        allowClear
        placeholder="账号类型"
        value={f.type}
        onChange={(type?: string) => set({ type })}
        options={[
          { value: 'personal', label: 'Claude 个人号' },
          { value: 'team-owner', label: 'Team 主号' },
          { value: 'team-member', label: 'Team 成员号' },
        ]}
        popupMatchSelectWidth={false}
        className="ai-filter-select"
      />
      {teams.length > 0 && (
        <Select
          allowClear
          showSearch
          placeholder="Team"
          value={f.team}
          onChange={(team?: string) => set({ team })}
          options={teams.map((t) => ({ value: t, label: t }))}
          popupMatchSelectWidth={false}
          className="ai-filter-select"
        />
      )}
      <Select
        allowClear
        showSearch
        placeholder="邮箱域名"
        value={f.domain}
        onChange={(domain?: string) => set({ domain })}
        options={domains.map((d) => ({ value: d, label: `@${d}` }))}
        popupMatchSelectWidth={false}
        className="ai-filter-select"
      />
      <Select<Status>
        placeholder="状态"
        value={f.status || undefined}
        allowClear
        onChange={(status?: Status) => set({ status: status ?? '' })}
        options={[
          { value: 'problem', label: '需处理' },
          { value: 'expiring', label: `订阅 ${expirySoon} 天内到期` },
          { value: 'no-token', label: '没有令牌' },
          { value: 'no-mail', label: '没有设置收信' },
          { value: 'no-device', label: '没有分配客户端' },
        ]}
        popupMatchSelectWidth={false}
        className="ai-filter-select"
      />
      {active && (
        <Button type="link" size="small" onClick={() => onChange({ ...emptyFilters, kind: f.kind })}>
          清除筛选
        </Button>
      )}
      <span className="hint ai-filter-count">
        {shown} / {items.length}
      </span>
    </Card>
  )
}

export default function AiAccounts() {
  const { message, modal } = App.useApp()
  const [claude, setClaude] = useState<ClaudeAccount[]>([])
  const [gemini, setGemini] = useState<GeminiAccount[]>([])
  const [devices, setDevices] = useState<Device[]>([])
  const [users, setUsers] = useState<User[]>([])
  const [gmail, setGmail] = useState<GmailClient>()
  const [mailServer, setMailServer] = useState<MailServer>()
  const [oauth, setOAuth] = useState<GeminiOAuthInfo>()
  const [overview, setOverview] = useState<GeminiOverview>()
  const [online, setOnline] = useState<AIOnlineSummary[]>([])
  const [loading, setLoading] = useState(true)
  const [filters, setFilters] = useState<Filters>(loadFilters)

  const [editClaude, setEditClaude] = useState<ClaudeAccount | AccountKind>()
  const [editGemini, setEditGemini] = useState<GeminiAccount | 'new'>()
  const [credFor, setCredFor] = useState<Item>()
  const [authorizing, setAuthorizing] = useState<ClaudeAccount>()
  const [gmailOpen, setGmailOpen] = useState(false)
  const [mailOpen, setMailOpen] = useState(false)
  const [oauthOpen, setOAuthOpen] = useState(false)

  const changeFilters = (f: Filters) => {
    setFilters(f)
    try {
      localStorage.setItem(filterKey, JSON.stringify(f))
    } catch {
      // 浏览器不让存时只是不记住
    }
  }

  const reload = useCallback(
    () =>
      Promise.all([
        serverApi.claudeAccounts(),
        serverApi.geminiAccounts(),
        serverApi.devices(),
        serverApi.users(),
        serverApi.gmailClient(),
        serverApi.geminiOAuth(),
        serverApi.geminiOverview(),
        serverApi.mailServer(),
      ])
        .then(([c, g, d, us, gc, o, v, ms]) => {
          setClaude(c)
          setGemini(g)
          setDevices(d)
          setUsers(us)
          setGmail(gc)
          setOAuth(o)
          setOverview(v)
          setMailServer(ms)
        })
        .catch((e: Error) => message.error(e.message))
        .finally(() => setLoading(false)),
    [message],
  )

  useEffect(() => {
    void reload()
  }, [reload])

  // 在线时长每分钟刷新；取不到（管理服务旧版本）时卡片上不显示，不影响其他内容。
  useEffect(() => {
    const load = () => serverApi.aiOnlineSummary().then(setOnline, () => setOnline([]))
    void load()
    const t = setInterval(load, 60000)
    return () => clearInterval(t)
  }, [])

  // 有进行中的鉴权测试或续期时每 3 秒刷新一次，直到出结果（客户端最多 90 秒内回报，超时由服务端判失败）。
  const testing =
    [...claude, ...gemini].some((a) => a.auth_test?.status === 'pending') ||
    claude.some((a) => a.refresh?.phase === 'notice' || a.refresh?.phase === 'running')
  useEffect(() => {
    if (!testing) return
    const t = setInterval(() => void reload(), 3000)
    return () => clearInterval(t)
  }, [testing, reload])

  const items = useMemo<Item[]>(
    () => [...claude.map((a) => ({ kind: a.kind, a }) as Item), ...gemini.map((a) => ({ kind: 'gemini', a }) as Item)],
    [claude, gemini],
  )
  const shown = useMemo(() => items.filter((it) => matches(it, filters)), [items, filters])
  const gmailReady = !!gmail?.client_id && !!gmail.has_secret
  const mailReady = !!mailServer?.host
  // 只有 Gmail 邮箱且没设置自建邮箱的账号才用得上 Gmail 授权；全是其他邮箱时不提示设置 Gmail 客户端。
  const needGmail = items.some((it) => it.kind !== 'gemini' && isGmail(it.a.email) && !it.a.has_mailbox)
  const needMail = items.some((it) => it.kind !== 'gemini' && it.a.has_mailbox)
  const deviceName = (id: number) => devices.find((d) => d.id === id)?.name ?? `#${id}`
  const expiring = items.filter((it) => {
    const left = expiryDays(it.a.sub_expires)
    return left !== undefined && left <= expirySoon
  })

  const startTest = (it: Item) =>
    (it.kind === 'gemini' ? serverApi.authTestGemini(it.a.id) : serverApi.authTestClaude(it.a.id))
      .then((t) => message.info(`已交给「${t.device_name}」测试，稍候出结果`))
      .catch((e: Error) => message.error(e.message))
      .finally(reload)

  const refreshNow = (a: ClaudeAccount) =>
    serverApi
      .refreshClaude(a.id)
      .then(() => message.info(a.kind === 'claude' ? '已通知客户端，最多 60 秒后续期' : '已交给客户端续期'))
      .catch((e: Error) => message.error(e.message))
      .finally(reload)

  const testMail = (a: ClaudeAccount) => {
    const what = a.mail_source === 'imap' ? '邮箱登录' : 'Gmail 授权'
    const hide = message.loading(`正在测试${what}…`, 0)
    return serverApi
      .testClaudeAccount(a.id)
      .then((r) => (r.ok ? message.success(`${a.email}：${what}有效`) : message.error(r.error ?? `${what}无效`)))
      .catch((e: Error) => message.error(e.message))
      .finally(hide)
  }

  const clearMailbox = (a: ClaudeAccount) =>
    modal.confirm({
      title: `清除「${a.email}」的邮箱密码？`,
      content: a.gmail_authorized ? '之后改用 Gmail 授权读登录邮件。' : '之后管理服务不能再读这个账号的登录邮件。',
      okText: '清除',
      okButtonProps: { danger: true },
      cancelText: '取消',
      onOk: () =>
        serverApi
          .clearMailbox(a.id)
          .then(() => message.success('已清除'))
          .catch((e: Error) => message.error(e.message))
          .finally(reload),
    })

  const clearCred = (it: Item) =>
    modal.confirm({
      title: `清除「${it.a.email}」托管的令牌？`,
      content:
        it.kind === 'gemini'
          ? '分配到的客户端会删除本机的 Antigravity 凭据，需要重新登录或重新填写。'
          : '客户端工作区里已经写入的登录不会删除；之后本机刷新的令牌不再上传，除非重新填写。',
      okText: '清除',
      okButtonProps: { danger: true },
      cancelText: '取消',
      onOk: () =>
        (it.kind === 'gemini' ? serverApi.clearGeminiCredential(it.a.id) : serverApi.clearClaudeCredential(it.a.id))
          .then(() => message.success('已清除'))
          .catch((e: Error) => message.error(e.message))
          .finally(reload),
    })

  const remove = (it: Item) =>
    modal.confirm({
      title: `删除「${it.a.email}」？`,
      content: it.kind === 'gemini' ? '服务端保存的凭据会一起删除，分配到的客户端会删除本机凭据。' : '服务端保存的 Gmail 授权、邮箱密码和托管的令牌会一起删除。',
      okText: '删除',
      okButtonProps: { danger: true },
      cancelText: '取消',
      onOk: () =>
        (it.kind === 'gemini' ? serverApi.deleteGeminiAccount(it.a.id) : serverApi.deleteClaudeAccount(it.a.id))
          .then(() => message.success('已删除'))
          .catch((e: Error) => message.error(e.message))
          .finally(reload),
    })

  const missing = [
    gmail && !gmailReady && needGmail && { label: 'Google OAuth 客户端（读 Gmail 登录邮件）', button: '设置 Gmail', open: () => setGmailOpen(true) },
    mailServer && !mailReady && needMail && { label: '自建邮箱的 IMAP 服务器', button: '设置自建邮箱', open: () => setMailOpen(true) },
    oauth && !oauth.has_secret && { label: 'Antigravity OAuth 客户端（Gemini 登录和刷新）', button: '设置 Antigravity', open: () => setOAuthOpen(true) },
  ].filter((v): v is { label: string; button: string; open: () => void } => !!v)

  return (
    <Space orientation="vertical" size={12} style={{ width: '100%' }}>
      <PageHeader
        title="AI 账号"
        count={items.length}
        actions={
          <>
            <Dropdown
              trigger={['click']}
              menu={{
                items: [
                  { key: 'mail', icon: <CloudServerOutlined />, label: '自建邮箱（IMAP / BillionMail）', onClick: () => setMailOpen(true) },
                  { key: 'gmail', icon: <GoogleOutlined />, label: 'Google OAuth 客户端（Gmail）', onClick: () => setGmailOpen(true) },
                  { key: 'agy', icon: <GoogleOutlined />, label: 'Antigravity OAuth 客户端', onClick: () => setOAuthOpen(true) },
                ],
              }}
            >
              <Button icon={<SettingOutlined />}>设置</Button>
            </Dropdown>
            <Dropdown
              trigger={['click']}
              menu={{
                items: [
                  { key: 'claude', label: 'Claude 账号', onClick: () => setEditClaude('claude') },
                  { key: 'codex', label: 'Codex 账号（ChatGPT）', onClick: () => setEditClaude('codex') },
                  { key: 'gemini', label: 'Gemini 账号', onClick: () => setEditGemini('new') },
                ],
              }}
            >
              <Button type="primary" icon={<PlusOutlined />}>
                添加账号
              </Button>
            </Dropdown>
          </>
        }
      />

      {missing.length > 0 && (
        <Alert
          type="warning"
          showIcon
          title={`${missing.map((m) => m.label).join('、')} 还没有设置`}
          action={
            <Space>
              {missing.map((m) => (
                <Button key={m.button} size="small" onClick={m.open}>
                  {m.button}
                </Button>
              ))}
            </Space>
          }
        />
      )}

      {expiring.length > 0 && (
        <Alert
          type="warning"
          showIcon
          title={`${expiring.length} 个账号的订阅快到期或已过期`}
          action={
            filters.status !== 'expiring' && (
              <Button size="small" onClick={() => changeFilters({ ...emptyFilters, status: 'expiring' })}>
                只看这些
              </Button>
            )
          }
        />
      )}

      {!loading && items.length > 0 && <FilterBar items={items} f={filters} onChange={changeFilters} shown={shown.length} />}

      {loading ? (
        <Skeleton active paragraph={{ rows: 6 }} />
      ) : shown.length === 0 ? (
        <Card>
          <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={items.length === 0 ? '还没有账号' : '没有符合筛选条件的账号'}>
            {items.length > 0 && <Button onClick={() => changeFilters(emptyFilters)}>清除筛选</Button>}
          </Empty>
        </Card>
      ) : (
        // 不同种类的账号分开显示：Claude、Codex、Gemini 各一组，各有标题和自己的卡片网格。
        (['claude', 'codex', 'gemini'] as Kind[]).map((kind) => {
          const group = shown.filter((it) => it.kind === kind)
          if (group.length === 0) return null
          return (
            <section key={kind} className="ai-section">
              <div className="ai-section-head">
                <Tag color={kindTag[kind].color} className="ai-kind">
                  {kindTag[kind].label}
                </Tag>
                <span className="ai-section-count">{group.length} 个账号</span>
              </div>
              <div className="ai-grid">
                {group.map((it) => (
                  <AccountCard
                    key={`${it.kind === 'gemini' ? 'g' : 'c'}${it.a.id}`}
                    item={it}
                    usage={it.kind === 'gemini' ? overview?.usage.find((u) => u.account_id === it.a.id) : undefined}
                    online={online.find((o) => o.kind === it.kind && o.account_id === it.a.id)}
                    devices={devices}
                    users={users}
                    deviceName={deviceName}
                    gmailReady={gmailReady}
                    onTest={() => startTest(it)}
                    onCredential={() => setCredFor(it)}
                    onEdit={() => (it.kind === 'gemini' ? setEditGemini(it.a) : setEditClaude(it.a))}
                    onAuthorize={() => it.kind !== 'gemini' && setAuthorizing(it.a)}
                    onRefresh={() => it.kind !== 'gemini' && refreshNow(it.a)}
                    onTestMail={() => it.kind !== 'gemini' && testMail(it.a)}
                    onClearMailbox={() => it.kind !== 'gemini' && clearMailbox(it.a)}
                    onClear={() => clearCred(it)}
                    onDelete={() => remove(it)}
                    onTeam={(team) => changeFilters({ ...filters, team })}
                  />
                ))}
              </div>
            </section>
          )
        })
      )}

      {overview && overview.devices.length > 0 && (filters.kind === 'all' || filters.kind === 'gemini') && (
        <GeminiDevicesCard overview={overview} accounts={gemini} onChanged={reload} />
      )}

      {/* 按账号 / 类型重新挂载，「来源手动选过」等状态从头开始 */}
      <ClaudeEditModal
        key={typeof editClaude === 'object' ? editClaude.id : (editClaude ?? 'none')}
        account={editClaude}
        devices={devices}
        users={users}
        onClose={() => setEditClaude(undefined)}
        onSaved={(a, created, source, test) => {
          setEditClaude(undefined)
          void reload()
          if (!created) return
          // 新账号按选的来源接着做：Gmail 授权、测试邮箱登录、测试刚填的令牌，或说明怎么在客户端登录上报。
          if (source === 'gmail') {
            if (gmailReady) setAuthorizing(a)
            else modal.info({ title: '还没有设置 Gmail 的 OAuth 客户端', content: '在右上角「设置 → Google OAuth 客户端（Gmail）」设置后，再在账号上点「授权 Gmail」。' })
          } else if (source === 'mailbox') {
            if (mailReady) void testMail(a)
            else modal.info({ title: '还没有设置自建邮箱的 IMAP 服务器', content: '在右上角「设置 → 自建邮箱」填好服务器后，在账号的「更多 → 测试收信」检查邮箱密码。' })
          } else if (source === 'token') {
            if (test) void startTest({ kind: a.kind, a })
          } else {
            modal.info({
              title: '在客户端上登录后自动上报',
              width: 520,
              content: (
                <ol className="simulate-list">
                  <li>在勾选的客户端上打开 Banbu Connect，首页「账号」里会出现 {a.email}。</li>
                  <li>
                    那台电脑上已经用 {a.kind === 'codex' ? 'codex' : 'claude'} 登录过这个账号时，卡片上会提示「本机已登录了这个账号」，点「使用并上报」；
                    否则点卡片上的「登录」{a.kind === 'codex' ? '（codex 会打开浏览器，按账号的注册方式登录即可）' : ''}。
                  </li>
                  <li>登录成功后客户端几秒内把凭据上报到这里，账号显示「已托管令牌」，其他分配到的客户端也会收到。</li>
                </ol>
              ),
            })
          }
        }}
      />
      <GeminiEditModal
        account={editGemini}
        devices={devices}
        users={users}
        onClose={() => setEditGemini(undefined)}
        onSaved={() => {
          setEditGemini(undefined)
          void reload()
        }}
      />
      <CredentialModal
        key={credFor ? `${credFor.kind}${credFor.a.id}` : 'none'}
        item={credFor}
        onClose={() => setCredFor(undefined)}
        onSaved={(test) => {
          const it = credFor
          setCredFor(undefined)
          if (test && it) void startTest(it)
          else void reload()
        }}
      />
      <ClientModal
        open={gmailOpen}
        client={gmail}
        onClose={() => setGmailOpen(false)}
        onSaved={(c) => {
          setGmail(c)
          setGmailOpen(false)
        }}
      />
      <MailServerModal
        open={mailOpen}
        server={mailServer}
        onClose={() => setMailOpen(false)}
        onSaved={(m) => {
          setMailServer(m)
          setMailOpen(false)
        }}
      />
      <OAuthModal
        open={oauthOpen}
        info={oauth}
        onClose={() => setOAuthOpen(false)}
        onSaved={(o) => {
          setOAuth(o)
          setOAuthOpen(false)
        }}
      />
      {/* 按账号重新挂载，换账号时输入和步骤状态从头开始 */}
      <AuthorizeModal key={authorizing?.id} account={authorizing} onClose={() => setAuthorizing(undefined)} onDone={reload} />
    </Space>
  )
}

// ---- 账号卡片 ----

// 底部状态行的一项：图标 + 短文字，细节在提示里。
function Chip({ icon, text, tip, tone }: { icon: ReactNode; text: ReactNode; tip?: ReactNode; tone?: 'ok' | 'warn' | 'error' | 'busy' }) {
  const chip = (
    <span className={`ai-chip${tone ? ` ai-chip-${tone}` : ''}`}>
      {icon}
      <span>{text}</span>
    </span>
  )
  return tip ? <Tooltip title={tip}>{chip}</Tooltip> : chip
}

function AccountCard({
  item,
  usage,
  online,
  devices,
  users,
  deviceName,
  gmailReady,
  onTest,
  onCredential,
  onEdit,
  onAuthorize,
  onRefresh,
  onTestMail,
  onClearMailbox,
  onClear,
  onDelete,
  onTeam,
}: {
  item: Item
  usage?: GeminiUsage
  online?: AIOnlineSummary
  devices: Device[]
  users: User[]
  deviceName: (id: number) => string
  gmailReady: boolean
  onTest: () => void
  onCredential: () => void
  onEdit: () => void
  onAuthorize: () => void
  onRefresh: () => void
  onTestMail: () => void
  onClearMailbox: () => void
  onClear: () => void
  onDelete: () => void
  onTeam: (team: string) => void
}) {
  const { kind, a } = item
  const test = a.auth_test
  const ai = kind === 'gemini' ? undefined : (a as ClaudeAccount)
  const g = kind === 'gemini' ? (a as GeminiAccount) : undefined
  const issues = problems(item)

  const more = [
    ...(ai
      ? [
          {
            key: 'refresh',
            icon: <ReloadOutlined />,
            label: '立即续期',
            disabled: !a.has_credential || ai.refresh?.phase === 'notice' || ai.refresh?.phase === 'running',
            onClick: onRefresh,
          },
          {
            key: 'mailtest',
            icon: <MailOutlined />,
            label: '测试收信',
            disabled: !ai.mail_source,
            onClick: onTestMail,
          },
          {
            key: 'auth',
            icon: <GoogleOutlined />,
            label: ai.gmail_authorized ? '重新授权 Gmail' : '授权 Gmail',
            disabled: !gmailReady || !isGmail(ai.email),
            onClick: onAuthorize,
          },
          ...(ai.has_mailbox ? [{ key: 'mailclear', icon: <StopOutlined />, label: '清除邮箱密码', onClick: onClearMailbox }] : []),
        ]
      : []),
    {
      key: 'clear',
      icon: <StopOutlined />,
      label: '清除令牌',
      disabled: !a.has_credential,
      onClick: onClear,
    },
    { type: 'divider' as const },
    {
      key: 'delete',
      icon: <DeleteOutlined />,
      label: '删除账号',
      danger: true,
      onClick: onDelete,
    },
  ]

  // 令牌：版本、来源和过期时间放在提示里。
  const tokenChip = a.has_credential ? (
    <Chip
      icon={<KeyOutlined />}
      text={`v${a.cred_version}`}
      tone={ai && tokenExpired(ai.cred_expires) ? 'warn' : 'ok'}
      tip={[a.cred_source, short(a.cred_updated_at), ai && ai.cred_expires > 0 && tokenExpiry(ai.cred_expires)].filter(Boolean).join(' · ')}
    />
  ) : (
    <Chip icon={<KeyOutlined />} text={g ? '未登录' : '无令牌'} tone="warn" tip={g ? '在客户端登录或填入令牌' : '填入令牌，或在客户端登录后上报'} />
  )

  const testChip = !test ? (
    <Chip icon={<ApiOutlined />} text="未测试" />
  ) : test.status === 'pending' ? (
    <Chip icon={<SyncOutlined spin />} text="测试中" tone="busy" tip={`「${test.device_name}」测试中`} />
  ) : test.status === 'ok' ? (
    <Chip icon={<CheckCircleFilled />} text="鉴权有效" tone="ok" tip={`${test.device_name} · ${short(test.started_at)}`} />
  ) : (
    <Chip icon={<CloseCircleFilled />} text="鉴权失败" tone="error" tip={`${test.message || '失败'}（${short(test.started_at)}）`} />
  )

  const mailChip =
    ai &&
    (ai.mail_source === 'imap' ? (
      <Chip icon={<MailOutlined />} text="IMAP" tone="ok" tip={`自建邮箱收信，登录名 ${ai.mail_user || ai.email}`} />
    ) : ai.mail_source === 'gmail' ? (
      <Chip icon={<MailOutlined />} text="Gmail" tone="ok" tip="已授权 Gmail 读登录邮件" />
    ) : (
      <Chip
        icon={<MailOutlined />}
        text="不收信"
        tip="没有设置邮箱密码（自建邮箱），也没有授权 Gmail：客户端不能一键登录、看不到登录邮件；在客户端用自己的登录流程登录后凭据仍会上报。"
      />
    ))

  const left = expiryDays(a.sub_expires)
  const subChip =
    left === undefined ? null : (
      <Chip
        icon={<ClockCircleOutlined />}
        text={left < 0 ? `过期 ${-left} 天` : left === 0 ? '今天到期' : `剩 ${left} 天`}
        tone={left < 0 ? 'error' : left <= expirySoon ? 'warn' : undefined}
        tip={`订阅到期日 ${a.sub_expires}`}
      />
    )

  const deviceChip = (
    <Chip
      icon={<DesktopOutlined />}
      text={g ? `${a.devices.length}/${g.max_devices}` : a.devices.length}
      tone={a.devices.length === 0 ? 'warn' : undefined}
      tip={
        a.devices.length === 0 ? (
          '没有分配客户端'
        ) : (
          <>
            {a.devices.map(deviceName).join('、')}
            {a.users.length > 0 && <div>分配给用户：{a.users.map((id) => users.find((u) => u.id === id)?.name ?? `#${id}`).join('、')}</div>}
            {ai?.kind === 'codex' && ai.holder > 0 && (
              <div>
                Refresh Token 持有者：{deviceName(ai.holder)}
                {ai.holder_device === 0 ? '（自动）' : ''}
              </div>
            )}
          </>
        )
      }
    />
  )

  // 在线时长（按切换时间算）：今天的合计，提示里按客户端列出今天、7 天和此刻是否在用。
  const liveOn = online?.devices.filter((d) => d.online_since) ?? []
  const onlineChip = online && online.week > 0 && (
    <Chip
      icon={<FieldTimeOutlined />}
      text={liveOn.length > 0 ? `在线 · 今天 ${formatDuration(online.today)}` : `今天 ${formatDuration(online.today)}`}
      tone={liveOn.length > 0 ? 'ok' : undefined}
      tip={
        <>
          <div>
            在线时长：今天 {formatDuration(online.today)}，近 7 天 {formatDuration(online.week)}
          </div>
          {online.devices.map((d) => (
            <div key={d.device_id}>
              {d.device_name || `客户端 ${d.device_id}`}：{d.online_since ? `在用（${formatClock(d.online_since)} 起），` : ''}今天{' '}
              {formatDuration(d.today)}，7 天 {formatDuration(d.week)}
            </div>
          ))}
          <div style={{ opacity: 0.7 }}>明细在「遥测 → AI 账号在线」</div>
        </>
      }
    />
  )

  const team = ai?.account_type === 'team' ? ai : undefined

  return (
    <Card size="small" className={`ai-card ai-card-${kind}`}>
      <div className="ai-card-head">
        <Tag color={kindTag[kind].color} className="ai-kind">
          {kindTag[kind].label}
        </Tag>
        <span className="ai-card-email selectable" title={a.email}>
          {a.email}
        </span>
        <span className="ai-card-actions">
          <Tooltip title={a.has_credential ? '测试鉴权（由在线的客户端执行）' : '先填入令牌'}>
            <Button
              size="small"
              type="text"
              aria-label="测试鉴权"
              icon={<ApiOutlined />}
              disabled={!a.has_credential}
              loading={test?.status === 'pending'}
              onClick={onTest}
            />
          </Tooltip>
          <Tooltip title="填入令牌">
            <Button size="small" type="text" aria-label="填入令牌" icon={<KeyOutlined />} onClick={onCredential} />
          </Tooltip>
          <Tooltip title="编辑">
            <Button size="small" type="text" aria-label="编辑" icon={<EditOutlined />} onClick={onEdit} />
          </Tooltip>
          <Dropdown trigger={['click']} menu={{ items: more }}>
            <Button size="small" type="text" aria-label="更多" icon={<MoreOutlined />} />
          </Dropdown>
        </span>
      </div>

      {(a.plan || team || a.note || (ai?.kind === 'claude' && ai.account_type === 'personal')) && (
        <div className="ai-card-tags">
          {a.plan && <Tag className="ai-plan">{a.plan}</Tag>}
          {team ? (
            <Tooltip title={`Team「${team.team_name}」· 域名 @${teamDomain(team)} · 点击只看这个 Team`}>
              <Tag
                color={team.team_role === 'owner' ? 'gold' : 'geekblue'}
                icon={<TeamOutlined />}
                className="ai-team"
                onClick={() => onTeam(team.team_name)}
              >
                {team.team_name} · {team.team_role ? teamRoleName[team.team_role] : ''}
              </Tag>
            </Tooltip>
          ) : (
            ai?.kind === 'claude' && <Tag bordered={false}>个人号</Tag>
          )}
          {team && <span className="hint">@{teamDomain(team)}</span>}
          {a.note && (
            <span className="hint ai-card-note" title={a.note}>
              {a.note}
            </span>
          )}
        </div>
      )}

      {ai ? <QuotaBars quota={ai.quota} device={ai.quota_device} /> : <GeminiBars usage={usage} />}

      <div className="ai-chips">
        {tokenChip}
        {testChip}
        {ai && a.has_credential && <RefreshChip a={ai} />}
        {mailChip}
        {subChip}
        {deviceChip}
        {onlineChip}
      </div>

      <AssignedList
        devices={devices}
        users={users}
        effective={a.devices}
        direct={a.direct_devices}
        userIds={a.users}
        holder={ai?.kind === 'codex' ? ai.holder : undefined}
      />

      {issues.length > 0 && (
        <Tooltip title={issues.map((m) => <div key={m}>{m}</div>)}>
          <div className="ai-issue">
            <ExclamationCircleFilled /> <span>{issues.join('；')}</span>
          </div>
        </Tooltip>
      )}
    </Card>
  )
}

// 续期状态（ADR-071）：进行中、等待、失败，或最近一次成功的时间。
function RefreshChip({ a }: { a: ClaudeAccount }) {
  const r: AIRefreshState | undefined = a.refresh
  const until = (v?: string) => (v ? new Date(v).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' }) : '')
  if (r?.phase === 'notice') return <Chip icon={<SyncOutlined spin />} text="续期中" tone="busy" tip={`通知客户端中，${until(r.deadline)} 前续期`} />
  if (r?.phase === 'running') return <Chip icon={<SyncOutlined spin />} text="续期中" tone="busy" tip={`「${r.device_name}」续期中`} />
  if (r?.phase === 'waiting') return <Chip icon={<SyncOutlined />} text="等客户端" tone="warn" tip={r.message} />
  const err = r?.phase === 'failed' ? r.message : a.refresh_error
  if (err)
    return (
      <Chip
        icon={<SyncOutlined />}
        text={r?.permanent ? 'RT 失效' : '续期失败'}
        tone="error"
        tip={r?.permanent ? `${err}。需要在客户端重新登录，或重新填入令牌。` : `${err}。稍后自动重试。`}
      />
    )
  return <Chip icon={<SyncOutlined />} text="自动续期" tip={a.refresh_at ? `上次续期 ${short(a.refresh_at)}` : '到期前自动续期'} />
}

// Claude / Codex 的用量窗口：已用比例。
function QuotaBars({ quota, device }: { quota?: AIQuota; device: string }) {
  if (!quota) return <div className="ai-quota ai-quota-empty hint">暂无用量：客户端登录或测试鉴权后上报</div>
  return (
    <div className="ai-quota">
      {quota.windows.map((w) => (
        <div className="ai-quota-row" key={w.key}>
          <span className="ai-quota-label">{w.label}</span>
          <Progress percent={Math.round(w.used_percent)} size="small" showInfo={false} strokeColor={usedColor(w.used_percent)} />
          <span className="ai-quota-pct">{Math.round(w.used_percent)}%</span>
          <span className="hint ai-quota-reset" title="重置时间">{w.reset_at ? short(w.reset_at) : ''}</span>
        </div>
      ))}
      {quota.error && <div className="ai-quota-error">{quota.error}</div>}
      <div className="hint ai-quota-foot">{[quota.plan && `上报套餐 ${quota.plan}`, device, short(quota.checked_at)].filter(Boolean).join(' · ')}</div>
    </div>
  )
}

// Gemini 的额度按窗口给（Claude / Gemini 的 5 小时、每周等；旧数据是逐模型）：显示最紧的 4 项，换算成已用比例和 Claude / Codex 一致。
function GeminiBars({ usage }: { usage?: GeminiUsage }) {
  if (!usage?.known) return <div className="ai-quota ai-quota-empty hint">暂无用量：客户端登录或测试鉴权后上报</div>
  const buckets = [...usage.buckets].sort((x, y) => x.remaining - y.remaining).slice(0, 4)
  return (
    <div className="ai-quota">
      {buckets.map((b) => {
        const used = Math.round((1 - b.remaining) * 100)
        return (
          <div className="ai-quota-row" key={b.key}>
            <span className="ai-quota-label" title={b.label}>
              {b.label}
            </span>
            <Progress percent={used} size="small" showInfo={false} strokeColor={usedColor(used)} />
            <span className="ai-quota-pct">{used}%</span>
            <span className="hint ai-quota-reset" title="重置时间">{b.reset_at ? short(b.reset_at) : ''}</span>
          </div>
        )
      })}
      <div className="hint ai-quota-foot">
        {[
          usage.buckets.length > 4 && `共 ${usage.buckets.length} 项`,
          usage.burn_per_hour > 0 && `每小时用 ${Math.round(usage.burn_per_hour * 100)}%`,
          usage.exhaust_at && `预计 ${short(usage.exhaust_at)} 用完`,
          `采样于 ${short(usage.sampled_at)}`,
        ]
          .filter(Boolean)
          .join(' · ')}
      </div>
    </div>
  )
}

// ---- 填入令牌 ----

function CredentialModal({ item, onClose, onSaved }: { item?: Item; onClose: () => void; onSaved: (test: boolean) => void }) {
  const { message } = App.useApp()
  const [form] = Form.useForm<{
    access: string
    refresh: string
    test: boolean
  }>()
  const [saving, setSaving] = useState(false)
  const gemini = item?.kind === 'gemini'

  const save = async () => {
    if (!item) return
    const v = await form.validateFields()
    setSaving(true)
    try {
      if (item.kind === 'gemini') await serverApi.setGeminiCredential(item.a.id, v.access ?? '', v.refresh ?? '')
      else await serverApi.setClaudeCredential(item.a.id, v.access ?? '', v.refresh ?? '')
      message.success('已保存，分配到的客户端会在几秒内收到')
      onSaved(v.test && item.a.devices.length > 0)
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  const isJSON = (s?: string) => !!s?.trim().startsWith('{')

  return (
    <Modal
      title={`填入令牌：${item?.a.email ?? ''}`}
      open={!!item}
      okText="保存"
      cancelText="取消"
      confirmLoading={saving}
      onOk={save}
      onCancel={onClose}
      destroyOnHidden
      width={560}
    >
      <Form form={form} layout="vertical" preserve={false} initialValues={{ access: '', refresh: '', test: true }}>
        <Form.Item
          name="access"
          label="Access Token（OAuth Token）"
          extra={
            gemini
              ? '可选，一小时就过期；也可以粘贴 Antigravity 的完整凭据 JSON。'
              : item?.kind === 'codex'
                ? '也可以直接粘贴 ~/.codex/auth.json 的内容。'
                : '也可以直接粘贴 Claude Code 的凭据 JSON（{"claudeAiOauth": …}）。'
          }
        >
          <Input.TextArea rows={3} placeholder={gemini ? 'ya29.…' : item?.kind === 'codex' ? 'eyJhbGciOi…' : 'sk-ant-oat01-…'} spellCheck={false} />
        </Form.Item>
        <Form.Item
          name="refresh"
          label="Refresh Token"
          dependencies={['access']}
          rules={[
            ({ getFieldValue }) => ({
              validator: (_, v?: string) => {
                const access = getFieldValue('access') as string | undefined
                if (isJSON(access) || isJSON(v)) return Promise.resolve()
                if (gemini && !v?.trim()) return Promise.reject(new Error('Gemini 账号要填 Refresh Token'))
                if (!v?.trim() && !access?.trim()) return Promise.reject(new Error('至少填一个令牌'))
                return Promise.resolve()
              },
            }),
          ]}
          extra={
            gemini
              ? undefined
              : 'Refresh Token 只保存在管理服务（Codex 另交给持有者客户端），由管理服务在到期前安排续期；客户端只收到 Access Token。只填 Access Token 时过期后失效。'
          }
        >
          <Input.TextArea rows={2} placeholder={gemini ? '1//0…' : item?.kind === 'codex' ? 'rt_…' : 'sk-ant-ort01-…'} spellCheck={false} />
        </Form.Item>
        <Form.Item name="test" valuePropName="checked" style={{ marginBottom: 0 }}>
          <Checkbox disabled={!item?.a.devices.length}>保存后立即测试鉴权（由分配到的在线客户端执行）</Checkbox>
        </Form.Item>
      </Form>
    </Modal>
  )
}
