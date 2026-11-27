import { useCallback, useEffect, useState, type ReactNode } from 'react'
import { Alert, App, Button, Collapse, Drawer, Empty, Skeleton, Tag } from 'antd'
import { CopyOutlined, ExportOutlined, ReloadOutlined } from '@ant-design/icons'
import { api, type ClaudeAccountBrief, type ClaudeMail } from '../../api'

// 查看 Claude 账号 Gmail 里 Anthropic 发来的邮件（最近 7 天、最多 10 封），由管理服务读取。
// 正文是纯文本；登录链接单独列出，可以复制，或由托盘在默认浏览器打开。
// 不要求先点「登录」：自己手动登录（在浏览器或 Claude Code 里输入邮箱）时，也在这里取登录链接。

const time = (v: string) => new Date(v).toLocaleString()

export default function ClaudeMails({
  account,
  open,
  waiting,
  notice,
  onClose,
}: {
  account?: ClaudeAccountBrief
  open: boolean
  waiting?: boolean // 一键登录正在等登录邮件：自动刷新
  notice?: ReactNode // 顶部的说明（如 Claude Desktop 的登录步骤），替换默认的「正在等登录邮件」
  onClose: () => void
}) {
  const { message } = App.useApp()
  const [mails, setMails] = useState<ClaudeMail[]>()
  const [error, setError] = useState<string>()
  const [loading, setLoading] = useState(false)
  const [active, setActive] = useState<string[]>([])

  const load = useCallback(() => {
    if (!account) return
    api.claude
      .mails(account.id)
      .then(
        (list) => {
          setMails(list)
          setError(undefined)
          // 默认展开最新一封
          setActive((cur) => (cur.length === 0 && list.length > 0 ? [list[0].id] : cur))
        },
        (e: Error) => setError(e.message),
      )
      .finally(() => setLoading(false))
  }, [account])

  useEffect(() => {
    if (!open) return
    load()
    if (!waiting) return
    const timer = setInterval(load, 5000)
    return () => clearInterval(timer)
  }, [open, waiting, load])

  const openLink = async (url: string) => {
    try {
      await api.claude.openLink(url)
      message.success('已交给托盘程序，几秒内在默认浏览器打开')
    } catch (e) {
      message.error((e as Error).message)
    }
  }

  const copy = (text: string, what = '登录链接') =>
    navigator.clipboard.writeText(text).then(
      () => message.success(`已复制${what}`),
      () => message.error('复制失败，请手动选择'),
    )
  const codex = account?.kind === 'codex'
  const sender = codex ? 'OpenAI' : 'Anthropic'

  return (
    <Drawer
      title={account ? `${account.email} 的邮件` : '邮件'}
      open={open}
      size="large"
      onClose={onClose}
      afterOpenChange={(v) => {
        if (!v) {
          setMails(undefined)
          setActive([])
        }
      }}
      extra={
        <Button
          icon={<ReloadOutlined />}
          loading={loading}
          onClick={() => {
            setLoading(true)
            load()
          }}
        >
          刷新
        </Button>
      }
    >
      <p className="hint">
        {codex
          ? 'OpenAI 最近 7 天发到这个邮箱的邮件。Codex 登录要验证码时，把最新一封里的验证码填到浏览器里。'
          : 'Anthropic 最近 7 天发到这个邮箱的邮件。登录链接可以直接在浏览器打开，或复制到要登录的浏览器里。登录链接等同于密码，不要发给别人。'}
      </p>
      {notice ?? (waiting && <Alert type="info" showIcon title="正在等登录邮件，收到后会出现在这里" style={{ marginBottom: 16 }} />)}
      {error && <Alert type="error" showIcon title={error} style={{ marginBottom: 16 }} />}
      {!mails && !error ? (
        <Skeleton active paragraph={{ rows: 4 }} />
      ) : mails && mails.length === 0 ? (
        <Empty description={`最近 7 天没有 ${sender} 的邮件`} />
      ) : (
        <Collapse
          activeKey={active}
          onChange={(k) => setActive(k as string[])}
          items={(mails ?? []).map((m) => ({
            key: m.id,
            label: (
              <span className="claude-mail-title">
                <span>{m.subject || '（无主题）'}</span>
                {m.links.length > 0 && <Tag color="green">登录链接</Tag>}
                {m.codes?.map((c) => (
                  <Tag key={c} color="green">
                    验证码 {c}
                  </Tag>
                ))}
                <span className="hint">{time(m.date)}</span>
              </span>
            ),
            children: (
              <div className="claude-mail">
                <div className="hint selectable">发件人：{m.from}</div>
                {m.links.map((url) => (
                  <div key={url} className="claude-mail-link">
                    <code className="selectable">{url}</code>
                    <span className="page-toolbar">
                      <Button type="primary" size="small" icon={<ExportOutlined />} onClick={() => void openLink(url)}>
                        在浏览器打开
                      </Button>
                      <Button size="small" icon={<CopyOutlined />} onClick={() => void copy(url)}>
                        复制
                      </Button>
                    </span>
                  </div>
                ))}
                {m.codes?.map((c) => (
                  <div key={c} className="claude-mail-link">
                    <span className="mail-code selectable">{c}</span>
                    <Button size="small" icon={<CopyOutlined />} onClick={() => void copy(c, '验证码')}>
                      复制
                    </Button>
                  </div>
                ))}
                <pre className="claude-mail-text selectable">{m.text}</pre>
              </div>
            ),
          }))}
        />
      )}
    </Drawer>
  )
}
