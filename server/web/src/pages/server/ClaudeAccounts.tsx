import { useEffect, useState } from 'react'
import { Alert, App, AutoComplete, Button, Checkbox, Form, Input, InputNumber, Modal, Radio, Select, Space, Switch } from 'antd'
import { GoogleOutlined } from '@ant-design/icons'
import { serverApi, type AccountKind, type ClaudeAccount, type ClaudeAccountInput, type Device, type GmailClient, type MailServer } from '../../serverApi'

// Claude、Codex 账号的弹窗（页面在 AiAccounts.tsx）。两类账号和 Gemini 一样分开添加，建好后类型不能改。
// 登录信息四种来源（ADR-067）：客户端登录后上报（不限注册方式）；账号是 Gmail 时授权 Gmail，客户端一键登录时
// 服务端取登录链接（ADR-025）、验证码（ADR-053）；自建邮箱（BillionMail 等）填 IMAP 密码，服务端直接登录邮箱读登录邮件，
// 不走 OAuth（ADR-072）；手动填写令牌（ADR-061），新建时就可以直接粘贴。

// 新账号的登录信息从哪来。Gmail 只是其中一种：账号不一定是 Gmail 注册的（ChatGPT 还能用微软、Apple、邮箱密码等注册），
// 所以默认按邮箱判断，不是 Gmail 的走客户端登录后上报（ADR-061 的「本机登录」上传）。
export type CredSource = 'client' | 'gmail' | 'mailbox' | 'token'

// 常见套餐，输入框里可选，也可以自己填（只用于显示和筛选，不向服务商查询）。
export const planOptions: Record<AccountKind | 'gemini', string[]> = {
  claude: ['Pro', 'Max 5x', 'Max 20x', 'Team Standard', 'Team Premium', 'Enterprise', 'Free'],
  codex: ['Plus', 'Pro', 'Business', 'Enterprise', 'Free'],
  gemini: ['AI Pro', 'AI Ultra', 'Free'],
}

export function PlanField({ kind }: { kind: AccountKind | 'gemini' }) {
  return (
    <Form.Item name="plan" label="套餐" extra="可选，自己标记，用于卡片显示和筛选。">
      <AutoComplete
        allowClear
        style={{ width: 200 }}
        placeholder="如：Max 5x"
        options={planOptions[kind].map((v) => ({ value: v }))}
        filterOption={(input, o) => !!o?.value && String(o.value).toLowerCase().includes(input.toLowerCase())}
      />
    </Form.Item>
  )
}

export const emailDomain = (email?: string) => (email ?? '').split('@')[1]?.toLowerCase() ?? ''

// Claude 个人号 / Team 号；Team 号标主号或成员号、Team 名称和邮箱域名。
function TeamFields({ email }: { email?: string }) {
  const type = Form.useWatch('account_type') as string | undefined
  return (
    <>
      <Form.Item name="account_type" label="账号类型">
        <Radio.Group
          optionType="button"
          options={[
            { value: 'personal', label: '个人号' },
            { value: 'team', label: 'Team 号' },
          ]}
        />
      </Form.Item>
      {type === 'team' && (
        <div className="form-row">
          <Form.Item name="team_role" label="身份" rules={[{ required: true, message: '请选择' }]}>
            <Select
              style={{ width: 120 }}
              options={[
                { value: 'owner', label: '主号' },
                { value: 'member', label: '成员号' },
              ]}
            />
          </Form.Item>
          <Form.Item name="team_name" label="Team 名称" rules={[{ required: true, whitespace: true, message: '请填写 Team 名称' }]} style={{ flex: 1 }}>
            <Input maxLength={60} placeholder="如：Acme" />
          </Form.Item>
          <Form.Item name="team_domain" label="邮箱域名" style={{ flex: 1 }}>
            <Input placeholder={emailDomain(email) || 'example.com'} spellCheck={false} />
          </Form.Item>
        </div>
      )}
    </>
  )
}

// 自建邮箱的 IMAP 登录信息。修改时密码留空表示不变。
function MailboxFields({ email, account, required }: { email?: string; account?: ClaudeAccount; required: boolean }) {
  return (
    <div className="form-row">
      <Form.Item name="mail_user" label="邮箱登录名" extra="留空表示用账号邮箱登录" style={{ flex: 1 }}>
        <Input placeholder={email || 'name@example.com'} autoComplete="off" spellCheck={false} />
      </Form.Item>
      <Form.Item
        name="mail_password"
        label="邮箱密码"
        style={{ flex: 1 }}
        extra={account?.has_mailbox ? '已保存，留空表示不修改' : '加密保存在管理服务，只写不读'}
        rules={required ? [{ required: true, message: '请填写邮箱密码' }] : undefined}
      >
        <Input.Password autoComplete="new-password" spellCheck={false} />
      </Form.Item>
    </div>
  )
}
export const isGmail = (email?: string) => /@(gmail|googlemail)\.com$/i.test((email ?? '').trim())

export const kindNames: Record<AccountKind, string> = { claude: 'Claude', codex: 'Codex' }

const isJSON = (s?: string) => !!s?.trim().startsWith('{')

type FormValues = ClaudeAccountInput & { source: CredSource; test: boolean }

// 订阅到期日：浏览器自带的日期选择，值为 YYYY-MM-DD；只用于显示和提醒，不向服务商查询。
export function ExpiryField() {
  return (
    <Form.Item name="sub_expires" label="订阅到期日" extra="可选。只用来提醒续费（提醒提前几天在「通知」里设置），不会去服务商查询。">
      <Input type="date" style={{ width: 200 }} />
    </Form.Item>
  )
}

// 令牌两栏：两栏各填一个令牌，或在任意一栏粘贴凭据 JSON（Claude 的 {"claudeAiOauth": …}、Codex 的 auth.json）。
export function TokenFields({ kind, required }: { kind: AccountKind; required: boolean }) {
  return (
    <>
      <Form.Item
        name="access_token"
        label="Access Token（OAuth Token）"
        extra={kind === 'codex' ? '也可以直接粘贴 ~/.codex/auth.json 的内容，邮箱会从里面读出。' : '也可以直接粘贴 Claude Code 的凭据 JSON（{"claudeAiOauth": …}）。'}
      >
        <Input.TextArea rows={3} placeholder={kind === 'codex' ? 'eyJhbGciOi… 或 {"tokens": …}' : 'sk-ant-oat01-…'} spellCheck={false} />
      </Form.Item>
      <Form.Item
        name="refresh_token"
        label="Refresh Token"
        dependencies={['access_token']}
        rules={[
          ({ getFieldValue }) => ({
            validator: (_, v?: string) => {
              const access = getFieldValue('access_token') as string | undefined
              if (!required || isJSON(access) || isJSON(v) || v?.trim() || access?.trim()) return Promise.resolve()
              return Promise.reject(new Error('至少填一个令牌'))
            },
          }),
        ]}
        extra="Refresh Token 只保存在管理服务，由它在到期前安排续期；客户端只收到 Access Token。只填 Access Token 时过期后失效。"
      >
        <Input.TextArea rows={2} placeholder={kind === 'codex' ? 'rt_…' : 'sk-ant-ort01-…'} spellCheck={false} />
      </Form.Item>
    </>
  )
}

// 新建（account 为类型）和编辑。
export function ClaudeEditModal({
  account,
  devices,
  onClose,
  onSaved,
}: {
  account?: ClaudeAccount | AccountKind
  devices: Device[]
  onClose: () => void
  onSaved: (a: ClaudeAccount, created: boolean, source: CredSource, test: boolean) => void
}) {
  const { message } = App.useApp()
  const [form] = Form.useForm<FormValues>()
  const [saving, setSaving] = useState(false)
  const [sourceTouched, setSourceTouched] = useState(false)
  const email = Form.useWatch('email', form)
  const source = Form.useWatch('source', form)
  const access = Form.useWatch('access_token', form)
  const chosen = Form.useWatch('devices', form) as number[] | undefined
  // 弹窗按账号重新挂载（key），初始值就是这个账号的。
  const [mailboxOpen, setMailboxOpen] = useState(() => typeof account === 'object' && account.has_mailbox)
  const isNew = typeof account === 'string'
  const kind: AccountKind = typeof account === 'string' ? account : (account?.kind ?? 'claude')
  // Codex 粘贴 auth.json 时邮箱可以从 id_token 读出，允许留空。
  const emailOptional = isNew && kind === 'codex' && source === 'token' && isJSON(access)

  // 没手动选过时，按邮箱给默认值：Gmail 邮箱授权读登录邮件，其他的在客户端登录后上报。
  useEffect(() => {
    if (isNew && !sourceTouched && email) form.setFieldValue('source', isGmail(email) ? 'gmail' : 'client')
  }, [email, isNew, sourceTouched, form])

  useEffect(() => {
    if (!account) return
    form.setFieldsValue(
      typeof account === 'string'
        ? {
            kind: account,
            email: '',
            note: '',
            devices: [],
            sub_expires: '',
            holder_device: 0,
            plan: '',
            account_type: 'personal',
            team_role: '',
            team_name: '',
            team_domain: '',
            mail_user: '',
            mail_password: '',
            source: 'client',
            access_token: '',
            refresh_token: '',
            test: true,
          }
        : {
            kind: account.kind,
            email: account.email,
            note: account.note,
            devices: account.devices,
            sub_expires: account.sub_expires,
            holder_device: account.holder_device,
            plan: account.plan,
            account_type: account.account_type || 'personal',
            team_role: account.team_role,
            team_name: account.team_name,
            team_domain: account.team_domain,
            mail_user: account.mail_user,
            mail_password: '',
          },
    )
  }, [account, form])

  const save = async () => {
    const { source: src, test, ...values } = await form.validateFields()
    const input: ClaudeAccountInput = {
      ...values,
      kind,
      sub_expires: values.sub_expires ?? '',
      holder_device: values.holder_device ?? 0,
      plan: values.plan ?? '',
      mail_user: values.mail_user ?? '',
      mail_password: values.mail_password ?? '',
    }
    if (!isNew || src !== 'token') {
      delete input.access_token
      delete input.refresh_token
    }
    // 新建时只有选了自建邮箱才带邮箱登录信息；编辑时展开了「自建邮箱」才带。
    if ((isNew && src !== 'mailbox') || (!isNew && !mailboxOpen)) {
      input.mail_user = typeof account === 'object' ? account.mail_user : ''
      input.mail_password = ''
    }
    setSaving(true)
    try {
      const a = isNew ? await serverApi.createClaudeAccount(input) : await serverApi.updateClaudeAccount((account as ClaudeAccount).id, input)
      message.success(isNew && src === 'token' ? '已保存，分配到的客户端会在几秒内收到令牌' : '已保存')
      onSaved(a, isNew, src ?? 'client', !!test && src === 'token' && a.devices.length > 0)
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <Modal
      title={isNew ? `添加 ${kindNames[kind]} 账号` : `编辑 ${kindNames[kind]} 账号`}
      open={!!account}
      okText="保存"
      cancelText="取消"
      confirmLoading={saving}
      onOk={save}
      onCancel={onClose}
      destroyOnHidden
      width={source === 'token' && isNew ? 600 : 520}
    >
      <Form form={form} layout="vertical" preserve={false}>
        <Form.Item
          name="email"
          label={kind === 'codex' ? 'ChatGPT 账号邮箱' : 'Claude 账号邮箱'}
          extra={
            !isNew
              ? '修改邮箱后，托管的令牌、Gmail 授权和邮箱密码会清除。类型不能修改，要换类型请删除后重新添加。'
              : emailOptional
                ? '可以留空：从粘贴的 auth.json 里读出邮箱。'
                : '登录这个账号用的邮箱，不限 Gmail；自己域名的邮箱（如 BillionMail）可以让管理服务直接收信。'
          }
          rules={[{ required: !emailOptional, type: 'email', message: '请填写邮箱' }]}
        >
          <Input placeholder="name@example.com" autoComplete="off" spellCheck={false} />
        </Form.Item>
        {isNew && (
          <Form.Item name="source" label="登录信息从哪来">
            <Radio.Group
              className="cred-source"
              onChange={() => setSourceTouched(true)}
              options={[
                {
                  value: 'client',
                  label: (
                    <span>
                      客户端登录后上报
                      <span className="hint">
                        不限注册方式。在分配到的客户端上登录，或用那台电脑上已经登录的 {kind}，凭据自动上报到这里
                      </span>
                    </span>
                  ),
                },
                {
                  value: 'gmail',
                  disabled: !isGmail(email),
                  label: (
                    <span>
                      授权 Gmail 读登录邮件
                      <span className="hint">只限 Gmail 邮箱。客户端一键登录时，由这里取登录{kind === 'codex' ? '验证码' : '链接'}</span>
                    </span>
                  ),
                },
                {
                  value: 'mailbox',
                  label: (
                    <span>
                      自建邮箱收信（IMAP）
                      <span className="hint">
                        BillionMail 等自己的邮箱服务器。填邮箱密码，由这里直接登录邮箱取登录{kind === 'codex' ? '验证码' : '链接'}，不走 OAuth
                      </span>
                    </span>
                  ),
                },
                {
                  value: 'token',
                  label: (
                    <span>
                      直接填入令牌
                      <span className="hint">
                        粘贴 Access / Refresh Token，或{kind === 'codex' ? ' auth.json' : '凭据 JSON'}，和账号一起保存后下发给勾选的客户端
                      </span>
                    </span>
                  ),
                },
              ]}
            />
          </Form.Item>
        )}
        {isNew && source === 'mailbox' && <MailboxFields email={email} required />}
        {!isNew && (
          <Form.Item label="自建邮箱收信（IMAP）" style={{ marginBottom: mailboxOpen ? 0 : undefined }}>
            <Switch checked={mailboxOpen} onChange={setMailboxOpen} size="small" />
            <span className="hint" style={{ marginInlineStart: 8 }}>
              {typeof account === 'object' && account.has_mailbox ? '已设置；关闭后保存不会清除，清除在卡片「更多」菜单里' : '设置后优先于 Gmail 授权'}
            </span>
          </Form.Item>
        )}
        {!isNew && mailboxOpen && <MailboxFields email={email} account={account as ClaudeAccount} required={false} />}
        {isNew && source === 'token' && <TokenFields kind={kind} required />}
        <PlanField kind={kind} />
        {kind === 'claude' && <TeamFields email={email} />}
        <ExpiryField />
        <Form.Item name="note" label="备注">
          <Input maxLength={200} placeholder="如：主力、备用" />
        </Form.Item>
        <Form.Item
          name="devices"
          label="可用的客户端"
          extra={
            isNew && source === 'client'
              ? '至少勾选一台客户端：在它上面登录这个账号后，凭据才会上报到这里。'
              : '只有勾选的客户端能用这个账号、收到托管的令牌；新加入的客户端默认没有任何账号。'
          }
          rules={
            isNew && source === 'client' ? [{ required: true, type: 'array', min: 1, message: '至少勾选一台客户端，在它上面登录后上报' }] : undefined
          }
        >
          <Select
            mode="multiple"
            allowClear
            placeholder="选择客户端（不选则没有客户端能用）"
            options={devices.filter((d) => !d.revoked).map((d) => ({ value: d.id, label: d.name }))}
          />
        </Form.Item>
        {kind === 'codex' && (
          <Form.Item
            name="holder_device"
            label="持有 Refresh Token 的客户端"
            extra="这台客户端保存完整凭据，codex 在它上面自己续期；管理服务也会在令牌到期前交给它续期。其他客户端只拿 Access Token。"
          >
            <Select
              options={[
                { value: 0, label: '自动（可用客户端里的第一台）' },
                ...devices.filter((d) => !d.revoked && chosen?.includes(d.id)).map((d) => ({ value: d.id, label: d.name })),
              ]}
            />
          </Form.Item>
        )}
        {isNew && source === 'token' && (
          <Form.Item name="test" valuePropName="checked" style={{ marginBottom: 0 }}>
            <Checkbox>保存后立即测试鉴权（由勾选的在线客户端执行）</Checkbox>
          </Form.Item>
        )}
      </Form>
    </Modal>
  )
}

// 设置 Google OAuth 客户端（桌面应用类型）。
export function ClientModal({
  open,
  client,
  onClose,
  onSaved,
}: {
  open: boolean
  client?: GmailClient
  onClose: () => void
  onSaved: (c: GmailClient) => void
}) {
  const { message } = App.useApp()
  const [form] = Form.useForm<{ client_id: string; client_secret: string }>()
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    if (open) form.setFieldsValue({ client_id: client?.client_id ?? '', client_secret: '' })
  }, [open, client, form])

  const save = async () => {
    const v = await form.validateFields()
    setSaving(true)
    try {
      onSaved(await serverApi.setGmailClient(v.client_id, v.client_secret))
      message.success('已保存')
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <Modal title="Google OAuth 客户端（Gmail）" open={open} okText="保存" cancelText="取消" confirmLoading={saving} onOk={save} onCancel={onClose} destroyOnHidden width={560}>
      <ol className="setup-steps">
        <li>
          在 Google Cloud 控制台新建项目，启用 <b>Gmail API</b>。
        </li>
        <li>
          「OAuth 同意屏幕」选「外部」，添加范围 <code>gmail.readonly</code>，然后<b>发布为正式版</b>：停在「测试」状态时，授权 7
          天后失效。未经 Google 验证会显示警告页，自用可以继续。
        </li>
        <li>
          「凭据 → 创建 OAuth 客户端 ID」，类型选<b>桌面应用</b>，把客户端 ID 和密钥填到下面。
        </li>
      </ol>
      <Form form={form} layout="vertical" preserve={false}>
        <Form.Item name="client_id" label="客户端 ID" rules={[{ required: true, message: '请填写客户端 ID' }]}>
          <Input placeholder="xxxx.apps.googleusercontent.com" spellCheck={false} autoComplete="off" />
        </Form.Item>
        <Form.Item
          name="client_secret"
          label="客户端密钥"
          extra={client?.has_secret ? '留空表示不修改。换了客户端后，所有账号都要重新授权 Gmail。' : undefined}
          rules={client?.has_secret ? [] : [{ required: true, message: '请填写客户端密钥' }]}
        >
          <Input.Password placeholder="GOCSPX-…" autoComplete="new-password" spellCheck={false} />
        </Form.Item>
      </Form>
    </Modal>
  )
}

// 授权 Gmail：打开 Google 授权页，授权后浏览器会停在打不开的 127.0.0.1 地址，把地址粘贴回来。
export function AuthorizeModal({ account, onClose, onDone }: { account?: ClaudeAccount; onClose: () => void; onDone: () => void }) {
  const { message } = App.useApp()
  const [pasted, setPasted] = useState('')
  const [opening, setOpening] = useState(false)
  const [opened, setOpened] = useState(false)
  const [saving, setSaving] = useState(false)

  const openGoogle = async () => {
    if (!account) return
    setOpening(true)
    try {
      const { url } = await serverApi.authorizeGmail(account.id)
      window.open(url, '_blank', 'noopener')
      setOpened(true)
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setOpening(false)
    }
  }

  const finish = async () => {
    if (!account) return
    setSaving(true)
    try {
      await serverApi.finishGmail(account.id, pasted)
      message.success(`${account.email}：Gmail 已授权`)
      onClose()
      onDone()
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <Modal
      title={`授权 Gmail：${account?.email ?? ''}`}
      open={!!account}
      okText="完成授权"
      cancelText="取消"
      okButtonProps={{ disabled: !pasted.trim() }}
      confirmLoading={saving}
      onOk={finish}
      onCancel={onClose}
      destroyOnHidden
    >
      <ol className="setup-steps">
        <li>
          <Space wrap>
            <span>打开 Google 授权页，用 {account?.email} 登录并允许只读访问 Gmail。</span>
            <Button type={opened ? 'default' : 'primary'} icon={<GoogleOutlined />} loading={opening} onClick={openGoogle}>
              {opened ? '重新打开' : '打开授权页'}
            </Button>
          </Space>
        </li>
        <li>授权后浏览器会跳到一个打不开的 http://127.0.0.1 页面，这是正常的。把地址栏里的完整地址复制到下面。</li>
      </ol>
      <Input.TextArea
        rows={3}
        placeholder="http://127.0.0.1:53682/?state=…&code=…"
        value={pasted}
        onChange={(e) => setPasted(e.target.value)}
        spellCheck={false}
      />
    </Modal>
  )
}

// 自建邮箱的 IMAP 服务器（BillionMail、Mailcow 等）：所有用自建邮箱收信的账号共用，账号里只填邮箱密码。
export function MailServerModal({
  open,
  server,
  onClose,
  onSaved,
}: {
  open: boolean
  server?: MailServer
  onClose: () => void
  onSaved: (m: MailServer) => void
}) {
  const { message } = App.useApp()
  const [form] = Form.useForm<MailServer>()
  const [saving, setSaving] = useState(false)
  const security = Form.useWatch('security', form)

  useEffect(() => {
    if (open) form.setFieldsValue({ host: '', port: 993, security: 'tls', skip_verify: false, folders: 'INBOX,Junk', ...server })
  }, [open, server, form])

  const save = async () => {
    const v = await form.validateFields()
    setSaving(true)
    try {
      onSaved(await serverApi.setMailServer({ ...v, host: v.host ?? '', folders: v.folders ?? '' }))
      message.success('已保存')
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <Modal title="自建邮箱（IMAP）" open={open} okText="保存" cancelText="取消" confirmLoading={saving} onOk={save} onCancel={onClose} destroyOnHidden width={560}>
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        title="账号用自己域名的邮箱注册时，管理服务直接用 IMAP 登录邮箱读登录邮件，不需要 Gmail 授权。"
        description="BillionMail：在它的后台给每个账号建邮箱，服务器填邮件域名（或 mail.域名），加密方式 TLS、端口 993。管理服务要能连到这个端口。"
      />
      <Form form={form} layout="vertical" preserve={false}>
        <div className="form-row">
          <Form.Item name="host" label="服务器" extra="留空表示不用自建邮箱" style={{ flex: 1 }}>
            <Input placeholder="mail.example.com" spellCheck={false} autoComplete="off" />
          </Form.Item>
          <Form.Item name="security" label="加密">
            <Select
              style={{ width: 130 }}
              onChange={(v) => form.setFieldValue('port', v === 'tls' ? 993 : 143)}
              options={[
                { value: 'tls', label: 'TLS' },
                { value: 'starttls', label: 'STARTTLS' },
                { value: 'none', label: '不加密' },
              ]}
            />
          </Form.Item>
          <Form.Item name="port" label="端口">
            <InputNumber min={1} max={65535} style={{ width: 100 }} />
          </Form.Item>
        </div>
        {security !== 'none' && (
          <Form.Item name="skip_verify" valuePropName="checked" extra="BillionMail 还没配好证书（自签名）时打开；有正式证书时保持关闭。">
            <Checkbox>不校验证书</Checkbox>
          </Form.Item>
        )}
        {security === 'none' && <Alert type="warning" showIcon style={{ marginBottom: 16 }} title="不加密时邮箱密码明文传输，只在内网测试时使用。" />}
        <Form.Item name="folders" label="搜索的文件夹" extra="逗号分隔。登录邮件可能被判成垃圾邮件，默认同时搜 INBOX 和 Junk；不存在的文件夹会跳过。" style={{ marginBottom: 0 }}>
          <Input placeholder="INBOX,Junk" spellCheck={false} />
        </Form.Item>
      </Form>
    </Modal>
  )
}
