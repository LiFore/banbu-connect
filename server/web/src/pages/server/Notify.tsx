import { useCallback, useEffect, useState } from 'react'
import {
  App,
  Button,
  Card,
  Form,
  Input,
  InputNumber,
  Select,
  Space,
  Switch,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd'
import { BellOutlined, DeleteOutlined, PlusOutlined, SendOutlined, SyncOutlined } from '@ant-design/icons'
import {
  serverApi,
  type AlertRules,
  type NotifyConfig,
  type NotifyInput,
  type NotifyRecord,
  type TrafficRule,
  type TrafficTarget,
} from '../../serverApi'
import { formatBytes, PageHeader } from '../common'

// 通知（S1.12）：推送渠道（Bark、飞书群机器人）和用量提醒规则。管理服务每 5 分钟检查一次，
// 同一件事只提醒一次，条件恢复后或进入新的统计周期 / 额度窗口后才会再提醒。订阅按各自的间隔自动更新，节点有变化时自动发布。

const kindLabels: Record<string, { label: string; color?: string }> = {
  node_traffic: { label: '节点流量', color: 'red' },
  sub_usage: { label: '订阅流量', color: 'orange' },
  sub_expire: { label: '订阅到期', color: 'orange' },
  ai_quota: { label: 'AI 额度', color: 'purple' },
  ai_expire: { label: 'AI 订阅到期', color: 'purple' },
  ai_refresh: { label: 'AI 令牌续期', color: 'red' },
  sub_update: { label: '订阅更新', color: 'blue' },
  test: { label: '测试' },
}

export default function Notify() {
  return (
    <Space orientation="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader title="通知" />
      <ChannelsCard />
      <RulesCard />
      <LogCard />
    </Space>
  )
}

// ---- 推送渠道 ----

interface ChannelForm {
  bark_enabled: boolean
  bark_server: string
  bark_group: string
  bark_keys_text: string
  feishu_enabled: boolean
  feishu_hook: string
  feishu_secret: string
  panel_url: string
}

function ChannelsCard() {
  const { message } = App.useApp()
  const [form] = Form.useForm<ChannelForm>()
  const [config, setConfig] = useState<NotifyConfig>()
  const [busy, setBusy] = useState<'' | 'save' | 'test'>('')
  const barkOn = Form.useWatch('bark_enabled', form)
  const feishuOn = Form.useWatch('feishu_enabled', form)

  useEffect(() => {
    serverApi.notify().then(
      (c) => {
        setConfig(c)
        form.setFieldsValue({ ...c, bark_keys_text: '', feishu_hook: '', feishu_secret: '' })
      },
      (e: Error) => message.error(e.message),
    )
  }, [form, message])

  // 敏感项留空表示不改。
  const input = (v: ChannelForm): NotifyInput => ({
    bark_enabled: v.bark_enabled,
    bark_server: v.bark_server ?? '',
    bark_group: v.bark_group ?? '',
    bark_keys: v.bark_keys_text?.trim()
      ? v.bark_keys_text
          .split(/[\n,，\s]+/)
          .map((k) => k.trim())
          .filter(Boolean)
      : undefined,
    feishu_enabled: v.feishu_enabled,
    feishu_hook: v.feishu_hook?.trim() || undefined,
    feishu_secret: v.feishu_secret?.trim() || undefined,
    panel_url: v.panel_url ?? '',
  })

  const run = async (kind: typeof busy, f: (v: NotifyInput) => Promise<void>) => {
    setBusy(kind)
    try {
      await f(input(await form.validateFields()))
    } catch (e) {
      if (e instanceof Error) message.error(e.message)
    } finally {
      setBusy('')
    }
  }

  return (
    <Card
      title={
        <span className="card-title">
          <BellOutlined />
          推送渠道
        </span>
      }
      extra={
        <Space>
          <Button
            icon={<SendOutlined />}
            loading={busy === 'test'}
            onClick={() =>
              run('test', async (v) => {
                const r = await serverApi.testNotify(v)
                if (r.ok) message.success('已发送测试消息')
                else message.error(`发送失败：${r.error}`)
              })
            }
          >
            发送测试
          </Button>
          <Button
            type="primary"
            loading={busy === 'save'}
            onClick={() =>
              run('save', async (v) => {
                const c = await serverApi.saveNotify(v)
                setConfig(c)
                form.setFieldsValue({ bark_keys_text: '', feishu_hook: '', feishu_secret: '' })
                message.success('已保存')
              })
            }
          >
            保存
          </Button>
        </Space>
      }
    >
      <Form form={form} layout="vertical" requiredMark={false} className="notify-form">
        <div className="notify-channels">
          <section className="notify-channel">
            <div className="notify-channel-head">
              <span className="notify-channel-name">Bark</span>
              <span className="hint">iOS 推送</span>
              <span className="release-row-fill" />
              <Form.Item name="bark_enabled" valuePropName="checked" noStyle>
                <Switch size="small" aria-label="启用 Bark" />
              </Form.Item>
            </div>
            <Form.Item
              name="bark_keys_text"
              label="设备 key"
              extra={
                config?.bark_keys_masked.length
                  ? `已保存 ${config.bark_keys_masked.join('、')}；留空不改，填写则替换`
                  : '在 Bark 应用里复制推送地址或 key，多个设备换行分隔'
              }
            >
              <Input.TextArea autoSize={{ minRows: 1, maxRows: 4 }} placeholder="https://api.day.app/xxxxxxxx/ 或 xxxxxxxx" disabled={!barkOn} />
            </Form.Item>
            <div className="notify-pair">
              <Form.Item name="bark_server" label="服务器（可选）" tooltip="自建 Bark 服务器时填写，默认 https://api.day.app">
                <Input placeholder="https://api.day.app" disabled={!barkOn} />
              </Form.Item>
              <Form.Item name="bark_group" label="分组（可选）">
                <Input placeholder="Banbu Connect" disabled={!barkOn} />
              </Form.Item>
            </div>
          </section>
          <section className="notify-channel">
            <div className="notify-channel-head">
              <span className="notify-channel-name">飞书机器人</span>
              <span className="hint">群自定义机器人</span>
              <span className="release-row-fill" />
              <Form.Item name="feishu_enabled" valuePropName="checked" noStyle>
                <Switch size="small" aria-label="启用飞书" />
              </Form.Item>
            </div>
            <Form.Item
              name="feishu_hook"
              label="Webhook 地址"
              extra={config?.feishu_hook_set ? '已保存；留空不改' : '群设置 → 群机器人 → 添加自定义机器人，复制 Webhook 地址'}
            >
              <Input placeholder="https://open.feishu.cn/open-apis/bot/v2/hook/…" disabled={!feishuOn} autoComplete="off" />
            </Form.Item>
            <Form.Item
              name="feishu_secret"
              label="签名密钥（可选）"
              extra={config?.feishu_secret_set ? '已保存；留空不改' : '机器人安全设置里开启「签名校验」时填写'}
            >
              <Input.Password disabled={!feishuOn} autoComplete="new-password" />
            </Form.Item>
          </section>
        </div>
        <Form.Item name="panel_url" label="管理服务地址（可选）" tooltip="点开通知后打开的页面" style={{ marginBottom: 0 }}>
          <Input placeholder="https://connect.example.com" style={{ maxWidth: 420 }} />
        </Form.Item>
      </Form>
    </Card>
  )
}

// ---- 提醒规则 ----

function RulesCard() {
  const { message } = App.useApp()
  const [rules, setRules] = useState<AlertRules>()
  const [targets, setTargets] = useState<TrafficTarget[]>([])
  const [saving, setSaving] = useState(false)
  const [checking, setChecking] = useState(false)

  useEffect(() => {
    serverApi.alerts().then(setRules, (e: Error) => message.error(e.message))
    serverApi.alertTargets().then(setTargets, () => undefined)
  }, [message])

  if (!rules) return <Card loading title="提醒规则" />

  const set = (patch: Partial<AlertRules>) => setRules({ ...rules, ...patch })
  const setRule = (i: number, patch: Partial<TrafficRule>) =>
    set({ traffic: rules.traffic.map((r, j) => (j === i ? { ...r, ...patch } : r)) })
  const usage = (name: string, period: 'day' | 'month') => {
    const t = targets.find((x) => x.name === name)
    return t ? (period === 'day' ? t.day : t.month) : undefined
  }

  const save = async () => {
    setSaving(true)
    try {
      setRules(await serverApi.saveAlerts(rules))
      message.success('已保存')
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  const check = async () => {
    setChecking(true)
    try {
      const r = await serverApi.checkAlerts()
      message.info(r.sent ? `发出 ${r.sent} 条提醒` : '没有新的提醒（已提醒过的不会重复发送）')
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setChecking(false)
    }
  }

  const targetOptions = [
    { label: '节点', options: targets.filter((t) => t.kind === 'node').map((t) => ({ value: t.name, label: t.name })) },
    { label: '代理组', options: targets.filter((t) => t.kind === 'group').map((t) => ({ value: t.name, label: t.name })) },
  ]

  return (
    <Card
      title={<span className="card-title">提醒规则</span>}
      extra={
        <Space>
          <Tooltip title="按已保存的规则立即检查一次">
            <Button icon={<SyncOutlined />} loading={checking} onClick={check}>
              立即检查
            </Button>
          </Tooltip>
          <Button type="primary" loading={saving} onClick={save}>
            保存
          </Button>
        </Space>
      }
    >
      <div className="alert-sections">
        <section>
          <div className="alert-section-head">
            <Typography.Text strong>节点流量</Typography.Text>
            <span className="hint">经过节点或代理组的代理流量（客户端遥测上报，链式代理的每一跳都算），按自然日或自然月累计</span>
          </div>
          <Table<TrafficRule>
            size="small"
            rowKey={(r) => r.id ?? `${r.name}-${rules.traffic.indexOf(r)}`}
            dataSource={rules.traffic}
            pagination={false}
            scroll={{ x: 640 }}
            locale={{ emptyText: '还没有规则' }}
            columns={[
              {
                title: '节点 / 代理组',
                key: 'name',
                render: (_, r, i) => (
                  <Select
                    showSearch
                    value={r.name || undefined}
                    placeholder="选择或输入名称"
                    options={targetOptions}
                    onChange={(v) => setRule(i, { name: v })}
                    style={{ width: '100%', minWidth: 180 }}
                  />
                ),
              },
              {
                title: '周期',
                key: 'period',
                width: 110,
                render: (_, r, i) => (
                  <Select
                    value={r.period}
                    onChange={(v) => setRule(i, { period: v })}
                    options={[
                      { value: 'day', label: '每天' },
                      { value: 'month', label: '每月' },
                    ]}
                    style={{ width: 96 }}
                  />
                ),
              },
              {
                title: '上限',
                key: 'limit',
                width: 150,
                render: (_, r, i) => (
                  <InputNumber
                    min={0.01}
                    step={1}
                    value={r.limit_gb}
                    suffix="GB"
                    onChange={(v) => setRule(i, { limit_gb: v ?? 0 })}
                    style={{ width: 130 }}
                  />
                ),
              },
              {
                title: '当前用量',
                key: 'usage',
                width: 150,
                render: (_, r) => {
                  const u = usage(r.name, r.period)
                  if (u === undefined) return <span className="hint">—</span>
                  const pct = r.limit_gb > 0 ? (u / (r.limit_gb * 1024 ** 3)) * 100 : 0
                  return (
                    <span className={pct >= 100 ? 'alert-usage over' : pct >= 80 ? 'alert-usage near' : 'alert-usage'}>
                      {formatBytes(u)}
                      {r.limit_gb > 0 && <span className="hint"> · {pct.toFixed(0)}%</span>}
                    </span>
                  )
                },
              },
              {
                title: '启用',
                key: 'enabled',
                width: 64,
                render: (_, r, i) => <Switch size="small" checked={r.enabled} onChange={(v) => setRule(i, { enabled: v })} />,
              },
              {
                key: 'del',
                width: 44,
                render: (_, _r, i) => (
                  <Tooltip title="删除">
                    <Button
                      size="small"
                      type="text"
                      danger
                      aria-label="删除规则"
                      icon={<DeleteOutlined />}
                      onClick={() => set({ traffic: rules.traffic.filter((_, j) => j !== i) })}
                    />
                  </Tooltip>
                ),
              },
            ]}
          />
          <Button
            type="dashed"
            icon={<PlusOutlined />}
            style={{ marginTop: 8 }}
            onClick={() => set({ traffic: [...rules.traffic, { name: '', period: 'month', limit_gb: 100, enabled: true }] })}
          >
            添加规则
          </Button>
        </section>

        <section>
          <div className="alert-section-head">
            <Typography.Text strong>订阅</Typography.Text>
            <span className="hint">机场在订阅响应里返回的已用流量和到期时间；填 0 表示不提醒这一项</span>
          </div>
          <div className="alert-fields">
            <Field label="已用流量达到">
              <InputNumber min={0} max={100} value={rules.sub_used_percent} suffix="%" onChange={(v) => set({ sub_used_percent: v ?? 0 })} />
            </Field>
            <Field label="剩余流量少于">
              <InputNumber min={0} value={rules.sub_remain_gb} suffix="GB" onChange={(v) => set({ sub_remain_gb: v ?? 0 })} />
            </Field>
            <Field label="离到期不到">
              <InputNumber min={0} value={rules.sub_expire_days} suffix="天" onChange={(v) => set({ sub_expire_days: v ?? 0 })} />
            </Field>
            <Field label="自动更新结果" hint="节点变化（自动发布后）、失败、需要手动确认">
              <Switch checked={rules.sub_update} onChange={(v) => set({ sub_update: v })} />
            </Field>
          </div>
        </section>

        <section>
          <div className="alert-section-head">
            <Typography.Text strong>AI 账号</Typography.Text>
            <span className="hint">
              客户端上报的额度：Claude / Codex 每个窗口（5 小时、7 天等）的已用比例，Gemini 每一项的已用比例；订阅到期日是在「AI 账号」里手动填写的
            </span>
          </div>
          <div className="alert-fields">
            <Field label="额度已用达到">
              <InputNumber min={0} max={100} value={rules.ai_used_percent} suffix="%" onChange={(v) => set({ ai_used_percent: v ?? 0 })} />
            </Field>
            <Field label="离订阅到期不到" hint="到期当天再提醒一次">
              <InputNumber min={0} value={rules.ai_expire_days} suffix="天" onChange={(v) => set({ ai_expire_days: v ?? 0 })} />
            </Field>
            <Field label="统计时区" hint="按日、按月的分界">
              <Input value={rules.timezone} onChange={(e) => set({ timezone: e.target.value })} style={{ width: 160 }} />
            </Field>
          </div>
        </section>
      </div>
    </Card>
  )
}

function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <label className="alert-field">
      <span className="alert-field-label">{label}</span>
      {children}
      {hint && <span className="hint">{hint}</span>}
    </label>
  )
}

// ---- 推送记录 ----

function LogCard() {
  const [list, setList] = useState<NotifyRecord[]>([])
  const [loading, setLoading] = useState(true)
  const load = useCallback(
    () =>
      serverApi
        .notifyLog()
        .then(setList, () => undefined)
        .finally(() => setLoading(false)),
    [],
  )
  useEffect(() => {
    void load()
    const t = setInterval(load, 30000)
    return () => clearInterval(t)
  }, [load])

  return (
    <Card size="small" title={<span className="card-title">最近的通知</span>} extra={<Button size="small" type="text" icon={<SyncOutlined />} onClick={() => void load()} aria-label="刷新" />}>
      <Table<NotifyRecord>
        size="small"
        rowKey="id"
        loading={loading}
        dataSource={list}
        pagination={{ pageSize: 20, hideOnSinglePage: true }}
        scroll={{ x: 720 }}
        locale={{ emptyText: '还没有通知' }}
        columns={[
          { title: '时间', dataIndex: 'created_at', width: 170, render: (v: string) => new Date(v).toLocaleString() },
          {
            title: '类型',
            dataIndex: 'kind',
            width: 100,
            render: (k: string) => <Tag color={kindLabels[k]?.color}>{kindLabels[k]?.label ?? k}</Tag>,
          },
          {
            title: '内容',
            key: 'content',
            render: (_, r) => (
              <div className="notify-log-content">
                <span className="notify-log-title">{r.title}</span>
                {r.body && <span className="hint">{r.body}</span>}
              </div>
            ),
          },
          {
            title: '推送',
            dataIndex: 'result',
            width: 200,
            render: (v: string) => (v ? <span className="notify-log-fail">{v}</span> : <Tag color="green">已送达</Tag>),
          },
        ]}
      />
    </Card>
  )
}
