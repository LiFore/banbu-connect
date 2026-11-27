import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { Alert, App, Button, Card, Dropdown, Empty, Input, Modal, Segmented, Skeleton, Space, Tag, Tooltip, type MenuProps } from 'antd'
import {
  ChromeOutlined,
  CodeOutlined,
  CopyOutlined,
  DeleteOutlined,
  DesktopOutlined,
  DownOutlined,
  EllipsisOutlined,
  LeftOutlined,
  LoginOutlined,
  MailOutlined,
  PlayCircleFilled,
  ReloadOutlined,
  RightOutlined,
  StarOutlined,
  SwapOutlined,
} from '@ant-design/icons'
import { Link, useNavigate } from 'react-router-dom'
import {
  api,
  type AccountsView,
  type AccountView,
  type AIKind,
  type ClaudeAccountBrief,
  type ClaudeLoginState,
  type GeminiAccountLocal,
  type GeminiLogin,
  type GeminiStatus,
  type WorkspaceAction,
  type WorkspaceActionKind,
} from '../../api'
import { PageHeader } from '../common'
import ClaudeMails from './ClaudeMails'
import VendorLogo from './VendorLogo'
import UsageList, { aiRows, geminiRows } from './UsageList'
import { openFullWindow } from './windowChrome'

type Kind = AIKind | 'gemini'

const kindLabel: Record<Kind, string> = { claude: 'Claude Code', codex: 'Codex', gemini: 'Antigravity' }
const cmdOf: Record<AIKind, string> = { claude: 'claude', codex: 'codex' }
const kindOrder: Kind[] = ['claude', 'codex', 'gemini']

const claudePhase: Record<ClaudeLoginState['phase'], string> = {
  launching: '正在启动 Claude Code…',
  waiting_mail: '已打开 Claude 登录页，正在等登录邮件',
  mail_ready: '登录邮件已到，请在「邮件」里点「在浏览器打开」',
  opened: '已在浏览器打开登录链接，请在浏览器里完成授权',
  done: '已登录',
  failed: '登录失败',
}

const geminiPhase: Record<GeminiLogin['phase'], string> = {
  opening: '正在打开浏览器档案…',
  authorizing: '已在浏览器档案里打开 Google 授权页，请登录并允许访问',
  exchanging: '已授权，正在保存凭据…',
  done: '已登录，凭据已同步到管理服务',
  failed: '登录失败',
}

const actionLabel: Record<WorkspaceActionKind, string> = { login: '登录', app_login: '用 ChatGPT 客户端登录', terminal: '打开终端', launch: '启动', set_default: '设为默认', delete: '删除', adopt_default: '用本机已登录的', desktop_app: '打开 Claude Desktop' }

const claudeLoginActive = (l?: ClaudeLoginState | null) => !!l && l.phase !== 'done' && l.phase !== 'failed'
const geminiLoginActive = (l?: GeminiLogin) => !!l && l.phase !== 'done' && l.phase !== 'failed'


// 账号页（BXC-F0-042）：客户端的首页。管理服务分配给本机的 Claude Code、Codex、Gemini（Antigravity）账号
// 放在同一组卡片里，不按类型分区；每张卡片最显眼的是「启动」（参考 cockpit-tools 的「切换并启动」）：
// Claude / Codex 在账号的隔离工作区打开终端并直接运行 claude / codex（多开，ADR-052）；
// Gemini 切到这个账号，托盘程序写好凭据后启动 Antigravity 应用（单开切换，ADR-046）。
// compact：嵌在精简主界面（窄窗口）里，和首页一样一排横向滚动，不显示类型筛选和命令行说明；
// 指向完整界面的链接（去绑定、去处理）在完整界面窗口里打开。
export default function Accounts({ embedded = false, compact = false }: { embedded?: boolean; compact?: boolean }) {
  embedded ||= compact
  const { message, modal } = App.useApp()
  const [view, setView] = useState<AccountsView>()
  const [gem, setGem] = useState<GeminiStatus>()
  const [error, setError] = useState<string>()
  const [login, setLogin] = useState<ClaudeLoginState | null>(null)
  const [mailFor, setMailFor] = useState<ClaudeAccountBrief>()
  // 正在辅助登录 Claude Desktop 的账号（ADR-089）：邮件抽屉自动刷新，并显示登录步骤。
  const [desktopFor, setDesktopFor] = useState<number>()
  const [busy, setBusy] = useState<string>()
  const [cwdFor, setCwdFor] = useState<{ kind: AIKind; email: string; action: 'terminal' | 'launch' }>()
  const [cwd, setCwd] = useState('')
  const [pasted, setPasted] = useState('')
  const [filter, setFilter] = useState<Kind | 'all'>('all')
  const [gemAt, setGemAt] = useState(0) // 读到 Gemini 状态的时间，用来判断启动请求是否还在等

  const refresh = useCallback(() => {
    api.accounts.view().then(
      (v) => {
        setView(v)
        setError(undefined)
      },
      (e: Error) => setError(e.message),
    )
    api.gemini.status().then(
      (s) => {
        setGem(s)
        setGemAt(Date.now())
      },
      () => undefined,
    )
    api.claude.current().then((r) => setLogin(r.login), () => undefined)
  }, [])

  const pending = !!view?.actions.some((a) => a.status === 'pending' || a.status === 'running')
  // 托盘程序 2 分钟内没有执行的启动请求作废（和后台服务一致）。
  const gemLaunching = !!gem?.launch && !gem.launch.done && gemAt - new Date(gem.launch.at).getTime() < 120_000
  const gemPending = gem?.pending && new Date(gem.pending.until).getTime() > gemAt ? gem.pending : undefined
  const fast = pending || claudeLoginActive(login) || geminiLoginActive(gem?.login) || gemLaunching || !!gemPending
  useEffect(() => {
    refresh()
    const timer = setInterval(refresh, fast ? 2000 : 10000)
    return () => clearInterval(timer)
  }, [refresh, fast])

  const showError = (e: unknown) => {
    message.error((e as Error).message)
  }

  const act = async (key: string, f: () => Promise<unknown>, done?: string) => {
    setBusy(key)
    try {
      await f()
      if (done) message.success(done)
      refresh()
    } catch (e) {
      showError(e)
    } finally {
      setBusy(undefined)
    }
  }

  const enqueue = (action: WorkspaceActionKind, kind: AIKind, email: string, extra: { cwd?: string } = {}) =>
    act(
      `${action}:${kind}:${email}`,
      () => api.accounts.act({ action, kind, email, ...extra }),
      action === 'terminal' ? '正在打开终端' : action === 'launch' ? `正在启动 ${cmdOf[kind]}` : action === 'desktop_app' ? '正在打开 Claude Desktop' : undefined,
    )

  // 所有账号放在一起：先按类型，再按邮箱。
  const cards = useMemo(() => {
    if (!view) return []
    const list: ({ kind: AIKind; a: AccountView } | { kind: 'gemini'; g: GeminiAccountLocal })[] = [
      ...view.accounts.map((a) => ({ kind: a.kind, a })),
      ...(gem?.accounts ?? []).map((g) => ({ kind: 'gemini' as const, g })),
    ]
    const email = (x: (typeof list)[number]) => ('a' in x ? x.a.email : x.g.email)
    return list.sort((x, y) => kindOrder.indexOf(x.kind) - kindOrder.indexOf(y.kind) || email(x).localeCompare(email(y)))
  }, [view, gem])

  if (error && !view) return <Alert type="error" showIcon title="连不上后台服务" description={error} />
  if (!view) {
    return (
      <div className="desktop-stack">
        <Card>
          <Skeleton active paragraph={{ rows: 4 }} />
        </Card>
      </div>
    )
  }

  const latest = (kind: AIKind, email: string) => view.actions.find((a) => a.kind === kind && a.email === email)
  const counts = (k: Kind) => cards.filter((c) => c.kind === k).length
  const shown = cards.filter((c) => filter === 'all' || c.kind === filter)
  const showDefaults = (k: AIKind) => filter === 'all' || filter === k

  const gl = gem?.login
  const launch = gem?.launch

  return (
    <div className="desktop-stack">
      <SectionHeader
        embedded={embedded}
        title="账号"
        count={cards.length}
        actions={
          <Space>
            {!compact && <Segmented<Kind | 'all'>
              size="small"
              value={filter}
              onChange={setFilter}
              options={[
                { value: 'all', label: '全部' },
                ...kindOrder.filter((k) => counts(k) > 0).map((k) => ({ value: k, label: `${kindLabel[k]} ${counts(k)}` })),
              ]}
            />}
            {!!gem?.accounts.length && (
              <Tooltip title="重新查询 Antigravity 额度">
                <Button
                  size="small"
                  icon={<ReloadOutlined />}
                  aria-label="重新查询额度"
                  loading={busy === 'quota'}
                  onClick={() =>
                    act('quota', async () => {
                      await api.gemini.refreshQuota()
                      await new Promise((r) => setTimeout(r, 3000))
                    })
                  }
                />
              </Tooltip>
            )}
          </Space>
        }
      />
      {!view.bound && <Alert type="info" showIcon title="未绑定管理服务，没有分配的账号" action={<GoTo to="/settings" compact={compact}>去绑定</GoTo>} />}
      {view.error && <Alert type="error" showIcon title={`读取账号失败：${view.error}`} />}
      {gem?.last_error && <Alert type="error" showIcon title={`同步 Antigravity 账号失败：${gem.last_error}`} />}
      {!view.shell && <Alert type="warning" showIcon title="托盘程序未运行，账号操作要等它启动后执行" />}
      {gemPending ? (
        <Alert
          type="warning"
          showIcon
          title={`${gemPending.reason}：Antigravity 正在运行，约 ${Math.max(0, Math.ceil((new Date(gemPending.until).getTime() - gemAt) / 1000))} 秒后关闭并重新打开`}
          description="请先保存进行中的工作"
          action={
            <Button size="small" type="primary" loading={busy === 'gemini:apply-now'} onClick={() => act('gemini:apply-now', () => api.gemini.applyNow(), '正在切换')}>
              立即切换
            </Button>
          }
        />
      ) : (
        gem?.rotated && <Alert type="info" showIcon title={gem.rotated} closable />
      )}
      {gem?.applied?.error && <Alert type="warning" showIcon title={`写入 Antigravity 本机凭据失败：${gem.applied.error}`} />}
      {launch && launch.done && launch.error && <Alert type="error" showIcon closable title={`启动 Antigravity（${launch.email}）失败：${launch.error}`} />}
      {login && (
        <Alert
          type={login.phase === 'failed' ? 'error' : login.phase === 'done' ? 'success' : 'info'}
          showIcon
          title={`${login.email}：${claudePhase[login.phase]}`}
          description={login.error || undefined}
        />
      )}
      {gl && (
        <Alert
          type={gl.phase === 'failed' ? 'error' : gl.phase === 'done' ? 'success' : 'info'}
          showIcon
          title={`${gl.email}：${geminiPhase[gl.phase]}`}
          description={
            gl.error ||
            (gl.phase === 'authorizing' && (
              <Space orientation="vertical" size={6} style={{ width: '100%' }}>
                <span>
                  授权后浏览器没有显示「已登录」时，把地址栏里 http://127.0.0.1 开头的地址粘贴到这里：
                </span>
                <Space.Compact style={{ width: '100%' }}>
                  <Input
                    size="small"
                    value={pasted}
                    placeholder="http://127.0.0.1:端口/oauth-callback?state=…&code=…"
                    spellCheck={false}
                    onChange={(e) => setPasted(e.target.value)}
                  />
                  <Button
                    size="small"
                    disabled={!pasted.trim()}
                    loading={busy === 'paste'}
                    onClick={() => act('paste', () => api.gemini.finishLogin(gl.id, pasted).then(() => setPasted('')), '已登录')}
                  >
                    完成登录
                  </Button>
                </Space.Compact>
              </Space>
            )) ||
            undefined
          }
        />
      )}

      <AccountStrip strip={embedded}>
        {shown.map((c) =>
          c.kind === 'gemini' ? (
            <GeminiAccountCard
              key={`gemini:${c.g.id}`}
              account={c.g}
              status={gem!}
              launching={gemLaunching && gem?.launch?.account_id === c.g.id}
              busy={busy}
              onLaunch={() =>
                act(`launch:gemini:${c.g.id}`, () => api.gemini.launch(c.g.id), `正在切换到 ${c.g.email} 并启动 Antigravity`)
              }
              onSwitch={() =>
                act(`switch:gemini:${c.g.id}`, () => api.gemini.setActive(c.g.id), `已切换到 ${c.g.email}，托盘程序正在写入凭据`)
              }
              onLogin={() => act(`login:gemini:${c.g.id}`, () => api.gemini.login(c.g.id))}
              onBrowser={() =>
                act(`browser:gemini:${c.g.id}`, async () => {
                  await api.gemini.openBrowser(c.g.id)
                  message.info('正在打开浏览器档案')
                })
              }
            />
          ) : (
            <AccountCard
              key={`${c.kind}:${c.a.id}`}
              account={c.a}
              last={latest(c.kind, c.a.email)}
              busy={busy}
              loginBusy={claudeLoginActive(login)}
              onLaunch={() => enqueue('launch', c.kind, c.a.email)}
              onLaunchAt={() => {
                setCwd('')
                setCwdFor({ kind: c.kind, email: c.a.email, action: 'launch' })
              }}
              onLogin={() => {
                // 没授权 Gmail 的账号（不一定是 Gmail 注册的）用 claude / codex 自己的登录流程，登录后自动上报（ADR-067）。
                if (c.kind === 'claude' && !c.a.mail) {
                  void enqueue('launch', c.kind, c.a.email)
                  message.info('已在终端运行 claude，登录后自动上报')
                  return
                }
                if (c.kind === 'claude') {
                  void act(`login:${c.kind}:${c.a.email}`, () => api.claude.login(c.a.id).then((r) => setLogin(r.login)))
                  return
                }
                void act(
                  `login:${c.kind}:${c.a.email}`,
                  async () => {
                    await api.accounts.act({ action: 'login', kind: c.kind, email: c.a.email })
                    if (c.a.mail) setMailFor({ id: c.a.id, kind: c.kind, email: c.a.email, note: c.a.note })
                  },
                  c.a.mail ? '正在打开浏览器登录，验证码看邮件' : '正在打开浏览器登录，登录后自动上报',
                )
              }}
              onAppLogin={() =>
                act(
                  `app_login:${c.kind}:${c.a.email}`,
                  () => api.accounts.act({ action: 'app_login', kind: c.kind, email: c.a.email }),
                  '正在打开 ChatGPT 客户端登录',
                )
              }
              onMail={() => setMailFor({ id: c.a.id, kind: c.kind, email: c.a.email, note: c.a.note })}
              onDesktopLogin={
                c.kind === 'claude'
                  ? () =>
                      act(`desktop_login:${c.kind}:${c.a.email}`, async () => {
                        // 先让托盘打开 Claude Desktop，成功后再复制邮箱、打开邮件等登录邮件。
                        await api.accounts.act({ action: 'desktop_app', kind: 'claude' })
                        const copied = await navigator.clipboard.writeText(c.a.email).then(
                          () => true,
                          () => false,
                        )
                        setDesktopFor(c.a.id)
                        setMailFor({ id: c.a.id, kind: c.kind, email: c.a.email, note: c.a.note })
                        message.info(copied ? `正在打开 Claude Desktop，邮箱 ${c.a.email} 已复制` : '正在打开 Claude Desktop')
                      })
                  : undefined
              }
              onTerminal={() => enqueue('terminal', c.kind, c.a.email)}
              onTerminalAt={() => {
                setCwd('')
                setCwdFor({ kind: c.kind, email: c.a.email, action: 'terminal' })
              }}
              onDefault={() =>
                modal.confirm({
                  title: `把 ${c.a.email} 设为默认？`,
                  content: `普通终端里的 ${cmdOf[c.kind]} 会改用这个账号。凭据从工作区移到默认目录；原来的默认账号是另一个工作区的会移回去，是你自己登录的会先备份。请先退出正在运行的 ${cmdOf[c.kind]}。`,
                  okText: '设为默认',
                  cancelText: '取消',
                  onOk: () => enqueue('set_default', c.kind, c.a.email),
                })
              }
              onDelete={() => enqueue('delete', c.kind, c.a.email)}
              localLogin={
                !c.a.workspace?.logged_in &&
                !view.defaults[c.kind]?.owner &&
                !!view.defaults[c.kind]?.logged_in &&
                view.defaults[c.kind]?.email?.toLowerCase() === c.a.email.toLowerCase()
              }
              onAdopt={() =>
                modal.confirm({
                  title: `用本机已登录的 ${kindLabel[c.kind]}？`,
                  content: `本机普通终端里的 ${cmdOf[c.kind]} 登录的就是 ${c.a.email}。确认后这份登录交给这个账号管理，并上报管理服务，分配到这个账号的其他客户端也能用。登录信息留在原处，普通终端里的 ${cmdOf[c.kind]} 照常可用。`,
                  okText: '使用并上报',
                  cancelText: '取消',
                  onOk: () => act(`adopt_default:${c.kind}:${c.a.email}`, () => api.accounts.act({ action: 'adopt_default', kind: c.kind, email: c.a.email }), '已交给这个账号，几秒后上报管理服务'),
                })
              }
            />
          ),
        )}
      </AccountStrip>
      {(['claude', 'codex'] as AIKind[]).some(showDefaults) && (
        <div className="account-defaults">
          {(['claude', 'codex'] as AIKind[])
            .filter(showDefaults)
            .map((kind) => (
              <DefaultCard
                key={`default:${kind}`}
                kind={kind}
                view={view}
                busy={busy}
                onLaunch={() => enqueue('launch', kind, '')}
                onTerminal={() => enqueue('terminal', kind, '')}
                onDesktopApp={kind === 'claude' ? () => enqueue('desktop_app', kind, '') : undefined}
              />
            ))}
        </div>
      )}
      {cards.length === 0 && <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="管理服务没有给本机分配账号" />}
      {gem && gem.accounts.length > 0 && (
        <p className="hint">
          {(() => {
            const active = gem.accounts.find((a) => a.id === gem.active_account_id)
            if (!active) return 'Antigravity 还没有可用账号，先登录一个'
            const ok = gem.applied && gem.applied.account_id === active.id && !gem.applied.error
            return ok ? `Antigravity 正在使用 ${active.email}` : `正在切换 Antigravity 到 ${active.email}…`
          })()}
        </p>
      )}
      {view.cli_dir && !compact && <CliLine dir={view.cli_dir} />}

      <ClaudeMails
        account={mailFor}
        open={!!mailFor}
        waiting={
          !!mailFor &&
          (mailFor.kind === 'codex' || desktopFor === mailFor.id || (!!login && login.account_id === mailFor.id && login.phase === 'waiting_mail'))
        }
        notice={
          mailFor && desktopFor === mailFor.id ? (
            <Alert
              type="info"
              showIcon
              title="登录 Claude Desktop"
              description={
                <ol className="desktop-login-steps">
                  <li>在 Claude Desktop（或它打开的浏览器登录页）选用邮箱登录，粘贴已复制的邮箱 {mailFor.email} 并发送。</li>
                  <li>登录邮件到了会出现在下面（每 5 秒刷新），点「在浏览器打开」或复制链接；邮件里是验证码时照着填。</li>
                  <li>浏览器里确认后按提示回到 Claude Desktop，完成登录。</li>
                </ol>
              }
              style={{ marginBottom: 16 }}
            />
          ) : undefined
        }
        onClose={() => {
          setMailFor(undefined)
          setDesktopFor(undefined)
        }}
      />
      <Modal
        title={cwdFor?.action === 'launch' ? `在指定目录启动 ${cwdFor ? cmdOf[cwdFor.kind] : ''}` : '在指定目录打开终端'}
        open={!!cwdFor}
        okText={cwdFor?.action === 'launch' ? '启动' : '打开'}
        cancelText="取消"
        onCancel={() => setCwdFor(undefined)}
        onOk={() => {
          if (cwdFor) void enqueue(cwdFor.action, cwdFor.kind, cwdFor.email, { cwd: cwd.trim() })
          setCwdFor(undefined)
        }}
        destroyOnHidden
      >
        <Input placeholder="代码仓库的完整路径，留空为用户目录" value={cwd} spellCheck={false} onChange={(e) => setCwd(e.target.value)} />
      </Modal>
    </div>
  )
}

// 卡片底部最显眼的「启动」按钮；旁边的下拉放次要的启动方式。
function LaunchButton({
  disabled,
  reason,
  loading,
  onClick,
  menu,
}: {
  disabled: boolean
  reason?: string
  loading: boolean
  onClick: () => void
  menu?: { label: string; onClick: () => void }
}) {
  const btn = menu ? (
    <Dropdown.Button
      type="primary"
      size="small"
      className="account-launch"
      disabled={disabled}
      loading={loading}
      icon={<DownOutlined />}
      menu={{ items: [{ key: 'more', label: menu.label }], onClick: menu.onClick }}
      onClick={onClick}
    >
      <PlayCircleFilled /> 启动
    </Dropdown.Button>
  ) : (
    <Button type="primary" size="small" className="account-launch" icon={<PlayCircleFilled />} disabled={disabled} loading={loading} onClick={onClick}>
      启动
    </Button>
  )
  return disabled && reason ? <Tooltip title={reason}>{btn}</Tooltip> : btn
}

// 卡片头部两行：厂商标识 | 邮箱（过长省略，悬停看全）+ 状态标签；第二行是类型和备注，一行放不下省略。
function CardHead({ kind, title, tags, sub }: { kind: Kind; title: string; tags: ReactNode; sub?: string }) {
  const line = [kindLabel[kind], sub].filter(Boolean).join(' · ')
  return (
    <div className="account-head">
      <VendorLogo kind={kind} size={22} />
      <Tooltip title={title} placement="topLeft">
        <span className="account-email selectable">{title}</span>
      </Tooltip>
      <span className="account-tags">{tags}</span>
      <span className="account-sub hint" title={line}>
        {line}
      </span>
    </div>
  )
}

function AccountCard({
  account: a,
  last,
  busy,
  loginBusy,
  onLaunch,
  onLaunchAt,
  onLogin,
  onAppLogin,
  onMail,
  onDesktopLogin,
  onTerminal,
  onTerminalAt,
  onDefault,
  onDelete,
  localLogin,
  onAdopt,
}: {
  account: AccountView
  last?: WorkspaceAction
  busy?: string
  loginBusy: boolean
  onLaunch: () => void
  onLaunchAt: () => void
  onLogin: () => void
  onAppLogin: () => void
  onMail: () => void
  onDesktopLogin?: () => void
  onTerminal: () => void
  onTerminalAt: () => void
  onDefault: () => void
  onDelete: () => void
  localLogin: boolean
  onAdopt: () => void
}) {
  const { modal } = App.useApp()
  const ws = a.workspace
  const key = (action: string) => `${action}:${a.kind}:${a.email}`
  const mismatch = ws?.logged_in && ws.login_email && ws.login_email.toLowerCase() !== a.email.toLowerCase()
  const more: MenuProps['items'] = [
    // 用 ChatGPT 桌面客户端登录：不需要装 codex 命令，客户端单开一个隔离实例，登录完自动关掉。
    ...(a.kind === 'codex' ? [{ key: 'app-login', icon: <LoginOutlined />, label: ws?.logged_in ? '用 ChatGPT 客户端重新登录' : '用 ChatGPT 客户端登录' }] : []),
    { key: 'mail', icon: <MailOutlined />, label: a.mail ? '邮件' : '邮件（未授权 Gmail）', disabled: !a.mail },
    // Claude Desktop 辅助登录：打开应用、复制邮箱、在邮件里等登录链接（要能读这个账号的邮件）。
    ...(onDesktopLogin
      ? [{ key: 'desktop-login', icon: <DesktopOutlined />, label: a.mail ? '登录 Claude Desktop' : '登录 Claude Desktop（未授权 Gmail）', disabled: !a.mail }]
      : []),
    { key: 'terminal-at', icon: <CodeOutlined />, label: '在指定目录打开终端…', disabled: !ws?.logged_in },
    ...(!ws?.default ? [{ key: 'default', icon: <StarOutlined />, label: '设为默认', disabled: !ws?.logged_in }] : []),
    ...(ws && !ws.default ? [{ type: 'divider' as const }, { key: 'delete', icon: <DeleteOutlined />, label: '删除工作区', danger: true }] : []),
  ]
  const onMore: MenuProps['onClick'] = ({ key: k }) => {
    if (k === 'app-login') onAppLogin()
    if (k === 'mail') onMail()
    if (k === 'desktop-login') onDesktopLogin?.()
    if (k === 'terminal-at') onTerminalAt()
    if (k === 'default') onDefault()
    if (k === 'delete')
      modal.confirm({
        title: '删除这个账号在本机的工作区？',
        content: '本机的登录和会话记录会删除，管理服务上的账号不变。',
        okText: '删除',
        okButtonProps: { danger: true },
        cancelText: '取消',
        onOk: onDelete,
      })
  }
  return (
    <Card size="small" className="account-card">
      <CardHead
        kind={a.kind}
        title={a.email}
        tags={
          <>
            {ws?.default && <Tag color="green">默认</Tag>}
            {!ws?.logged_in && <Tag color="orange">未登录</Tag>}
            {ws && ws.running > 0 && <Tag title="运行中的进程">运行 {ws.running}</Tag>}
          </>
        }
        sub={[a.quota?.plan, a.note, mismatch ? `实际登录 ${ws?.login_email}` : ''].filter(Boolean).join(' · ') || undefined}
      />
      <UsageList limit={2} rows={aiRows(a.quota)} error={a.quota?.error} empty={ws?.logged_in ? '暂无用量' : '登录后显示用量'} />
      {localLogin && (
        <div className="account-adopt">
          <span className="hint">本机已登录这个账号</span>
          <Button size="small" type="link" loading={busy === key('adopt_default')} onClick={onAdopt}>
            使用并上报
          </Button>
        </div>
      )}
      {last && last.status !== 'done' && (
        <span className="hint account-last" title={last.error || undefined}>
          {actionLabel[last.action]}：{last.status === 'failed' ? last.error : '进行中…'}
        </span>
      )}
      <div className="account-actions">
        <LaunchButton
          disabled={!ws?.logged_in}
          reason="先登录这个账号"
          loading={busy === key('launch')}
          onClick={onLaunch}
          menu={{ label: '在指定目录启动…', onClick: onLaunchAt }}
        />
        <span className="account-tools">
          <Tooltip title={ws?.logged_in ? '重新登录' : '登录'}>
            <Button size="small" icon={<LoginOutlined />} disabled={loginBusy && a.kind === 'claude' && a.mail} loading={busy === key('login')} onClick={onLogin} aria-label="登录" />
          </Tooltip>
          <Tooltip title="打开终端">
            <Button size="small" icon={<CodeOutlined />} disabled={!ws?.logged_in} loading={busy === key('terminal')} onClick={onTerminal} aria-label="打开终端" />
          </Tooltip>
          <Dropdown menu={{ items: more, onClick: onMore }} trigger={['click']}>
            <Button size="small" icon={<EllipsisOutlined />} loading={busy === key('set_default') || busy === key('delete') || busy === key('app_login') || busy === key('desktop_login')} aria-label="更多操作" />
          </Dropdown>
        </span>
      </div>
    </Card>
  )
}

// Gemini（Antigravity）账号（S1.7）：凭据由托盘程序写进 agy CLI 和 Antigravity 应用，同时只能用一个账号；
// 每个账号有固定的浏览器档案（ADR-047）。
// 「切换」只把本机当前账号换成它（应用在运行时由托盘程序重启它）；「启动」切换后再打开应用。
function GeminiAccountCard({
  account: a,
  status,
  launching,
  busy,
  onLaunch,
  onSwitch,
  onLogin,
  onBrowser,
}: {
  account: GeminiAccountLocal
  status: GeminiStatus
  launching: boolean
  busy?: string
  onLaunch: () => void
  onSwitch: () => void
  onLogin: () => void
  onBrowser: () => void
}) {
  const active = a.id === status.active_account_id
  const applied = active && status.applied?.account_id === a.id && !status.applied.error
  const key = (action: string) => `${action}:gemini:${a.id}`
  return (
    <Card size="small" className={active ? 'account-card account-card-active' : 'account-card'}>
      <CardHead
        kind="gemini"
        title={a.email}
        tags={
          <>
            {active && <Tag color="green">{applied ? '当前' : '切换中'}</Tag>}
            {!a.has_credential && <Tag color="orange">未登录</Tag>}
          </>
        }
        sub={a.note || undefined}
      />
      <UsageList limit={2} rows={geminiRows(a.quota)} error={a.quota?.error} empty={a.has_credential ? '暂无用量' : '登录后显示用量'} />
      {launching && <span className="hint account-last">正在启动 Antigravity…</span>}
      <div className="account-actions">
        <LaunchButton disabled={!a.has_credential} reason="先登录这个账号" loading={busy === key('launch') || launching} onClick={onLaunch} />
        <span className="account-tools">
          {!active && (
            <Tooltip title={a.has_credential ? '切到这个账号，不打开应用' : '先登录这个账号'}>
              <Button size="small" icon={<SwapOutlined />} disabled={!a.has_credential} loading={busy === key('switch')} onClick={onSwitch} aria-label="切换" />
            </Tooltip>
          )}
          <Tooltip title={a.has_credential ? '重新登录' : '登录'}>
            <Button size="small" icon={<LoginOutlined />} disabled={geminiLoginActive(status.login)} loading={busy === key('login')} onClick={onLogin} aria-label="登录" />
          </Tooltip>
          <Tooltip title="浏览器档案">
            <Button size="small" icon={<ChromeOutlined />} loading={busy === key('browser')} onClick={onBrowser} aria-label="浏览器档案" />
          </Tooltip>
        </span>
      </div>
    </Card>
  )
}

// 本机默认账号：普通终端里直接运行 claude / codex 时用的账号。
function DefaultCard({
  kind,
  view,
  busy,
  onLaunch,
  onTerminal,
  onDesktopApp,
}: {
  kind: AIKind
  view: AccountsView
  busy?: string
  onLaunch: () => void
  onTerminal: () => void
  // Claude Desktop 用它自己的登录，和这里的默认账号无关（ADR-088）；只有 Claude 有。
  onDesktopApp?: () => void
}) {
  const def = view.defaults[kind]
  const q = view.default_quota[kind]
  // 用量只给一行摘要：默认账号是次要信息，做成细长条，不和账号卡片一样大。
  const usage = q?.error ? `用量：${q.error}` : aiRows(q)?.map((r) => `${r.label} ${r.text}`).join(' · ')
  const who = def?.owner || def?.email || (def?.logged_in ? '自己登录的' : '')
  const line = usage || who || '未登录'
  const lastApp = onDesktopApp && view.actions.find((a) => a.kind === kind && a.action === 'desktop_app')
  return (
    <div className="account-default" title={`普通终端里直接运行 ${cmdOf[kind]} 时用的账号`}>
      <VendorLogo kind={kind} size={22} />
      <div className="account-default-text">
        <span className="account-default-title">
          {kindLabel[kind]} 默认
          {def && def.running > 0 && <Tag>运行 {def.running}</Tag>}
        </span>
        <span className="hint" title={[who, usage, lastApp?.error].filter(Boolean).join(' · ') || undefined}>
          {lastApp?.status === 'failed' ? `打开 Claude Desktop：${lastApp.error}` : line}
        </span>
      </div>
      <span className="account-tools">
        {onDesktopApp && (
          <Tooltip title="打开 Claude Desktop（用它自己登录的账号）">
            <Button size="small" icon={<DesktopOutlined />} loading={busy === `desktop_app:${kind}:`} onClick={onDesktopApp} aria-label="打开 Claude Desktop" />
          </Tooltip>
        )}
        <Tooltip title="打开终端">
          <Button size="small" icon={<CodeOutlined />} loading={busy === `terminal:${kind}:`} onClick={onTerminal} aria-label="打开终端" />
        </Tooltip>
        <LaunchButton disabled={!def?.logged_in} reason={`先在终端运行 ${cmdOf[kind]} 登录`} loading={busy === `launch:${kind}:`} onClick={onLaunch} />
      </span>
    </div>
  )
}

// 指向完整界面某页的链接：在精简主界面里打开完整界面窗口到那一页（没有外壳时在当前页跳过去）。
function GoTo({ to, compact, onClick, children }: { to: string; compact: boolean; onClick?: () => void; children: ReactNode }) {
  const navigate = useNavigate()
  if (!compact)
    return (
      <Link to={to} onClick={onClick}>
        {children}
      </Link>
    )
  return (
    <a
      href={to}
      onClick={(e) => {
        e.preventDefault()
        onClick?.()
        if (!openFullWindow(to)) navigate(to)
      }}
    >
      {children}
    </a>
  )
}

// 账号区标题：单独的账号页用页面标题，嵌在首页时用小一号的区块标题。
// 首页的账号卡片排成一行横向滚动（卡片等宽，按卡片对齐停下），不再按网格往下排：
// 状态在上、账号在下都能留在一屏里。鼠标滚轮的竖向滚动转成横向；两端还有内容时显示左右箭头。
// 单独的账号页（embedded 为 false）仍是网格。
function AccountStrip({ strip, children }: { strip: boolean; children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null)
  const [edge, setEdge] = useState({ left: false, right: false })
  const measure = useCallback(() => {
    const el = ref.current
    if (!el) return
    setEdge({ left: el.scrollLeft > 4, right: el.scrollLeft + el.clientWidth < el.scrollWidth - 4 })
  }, [])
  useEffect(() => {
    const el = ref.current
    if (!strip || !el) return
    measure()
    const onWheel = (e: WheelEvent) => {
      // 触控板本身有横向分量，不接管；只有竖向滚动、且这一行还能往那边滚时才转成横向，否则让页面滚。
      if (Math.abs(e.deltaX) >= Math.abs(e.deltaY)) return
      const max = el.scrollWidth - el.clientWidth
      if ((e.deltaY < 0 && el.scrollLeft <= 0) || (e.deltaY > 0 && el.scrollLeft >= max)) return
      e.preventDefault()
      el.scrollLeft += e.deltaY
    }
    el.addEventListener('wheel', onWheel, { passive: false })
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    return () => {
      el.removeEventListener('wheel', onWheel)
      ro.disconnect()
    }
  }, [strip, measure])
  useEffect(measure, [children, measure])
  if (!strip) return <div className="account-grid">{children}</div>
  const page = (dir: 1 | -1) => ref.current?.scrollBy({ left: dir * Math.max(240, ref.current.clientWidth - 80), behavior: 'smooth' })
  return (
    <div className="account-strip-wrap" data-left={edge.left || undefined} data-right={edge.right || undefined}>
      <div className="account-strip" ref={ref} onScroll={measure}>
        {children}
      </div>
      {edge.left && (
        <button type="button" className="strip-arrow strip-arrow-left" aria-label="向左滚动" onClick={() => page(-1)}>
          <LeftOutlined />
        </button>
      )}
      {edge.right && (
        <button type="button" className="strip-arrow strip-arrow-right" aria-label="向右滚动" onClick={() => page(1)}>
          <RightOutlined />
        </button>
      )}
    </div>
  )
}

function SectionHeader({ embedded, title, count, actions }: { embedded: boolean; title: string; count: number; actions: ReactNode }) {
  if (!embedded) return <PageHeader title={title} count={count} actions={actions} />
  return (
    <div className="section-head">
      <span className="section-head-title">
        {title}
        <span className="page-count">{count}</span>
      </span>
      {actions}
    </div>
  )
}

// 命令行入口（BXC-F0-038）：banbu-claude / banbu-codex <账号> 按工作区运行。首页只留一行说明和复制按钮。
function CliLine({ dir }: { dir: string }) {
  const { message } = App.useApp()
  const win = dir.includes('\\')
  // Windows 只改用户级 PATH（不用 setx：它会截断，并把系统 PATH 混进用户 PATH）。
  const pathLine = win
    ? `[Environment]::SetEnvironmentVariable('Path', [Environment]::GetEnvironmentVariable('Path', 'User') + ';${dir}', 'User')`
    : `echo 'export PATH="$PATH:${dir}"' >> ~/.zshrc`
  const copy = () => navigator.clipboard.writeText(pathLine).then(() => message.success('已复制，在终端运行一次即可'), () => message.error('复制失败'))
  return (
    <div className="account-cli hint">
      <span title="账号写邮箱开头几个字即可，其余参数原样传下去">
        命令行：<code>banbu-claude 账号</code> / <code>banbu-codex 账号</code>
      </span>
      <Tooltip title={pathLine}>
        <Button size="small" type="link" icon={<CopyOutlined />} onClick={() => void copy()}>
          复制 PATH 设置
        </Button>
      </Tooltip>
    </div>
  )
}
