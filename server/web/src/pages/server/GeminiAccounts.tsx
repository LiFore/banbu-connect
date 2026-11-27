import { useEffect, useState } from 'react'
import { App, Button, Card, Form, Input, InputNumber, Modal, Select, Space, Table, Tag } from 'antd'
import { SwapOutlined } from '@ant-design/icons'
import {
  serverApi,
  type Device,
  type GeminiAccount,
  type GeminiAccountInput,
  type GeminiDeviceView,
  type GeminiOAuthInfo,
  type GeminiOverview,
} from '../../serverApi'
import { ExpiryField, PlanField } from './ClaudeAccounts'

// Gemini（Antigravity）账号的弹窗和调度卡片（页面在 AiAccounts.tsx，ADR-046）：管理服务托管每个账号的凭据（加密），
// 分配给客户端；客户端登录一次后上传，或由管理员直接填 refresh token（ADR-061）。

const short = (v: string) => (v ? new Date(v).toLocaleString([], { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : '')

const isJSON = (s?: string) => !!s?.trim().startsWith('{')

// 新建和编辑。新建时可以直接填 Refresh Token 或粘贴凭据 JSON，不填则在客户端登录后上报。
export function GeminiEditModal({
  account,
  devices,
  onClose,
  onSaved,
}: {
  account?: GeminiAccount | 'new'
  devices: Device[]
  onClose: () => void
  onSaved: () => void
}) {
  const { message } = App.useApp()
  const [form] = Form.useForm<GeminiAccountInput>()
  const [saving, setSaving] = useState(false)
  const refresh = Form.useWatch('refresh_token', form)
  const isNew = account === 'new'

  useEffect(() => {
    if (!account) return
    form.setFieldsValue(
      account === 'new'
        ? { email: '', note: '', max_devices: 2, devices: [], sub_expires: '', plan: '', refresh_token: '' }
        : { email: account.email, note: account.note, max_devices: account.max_devices, devices: account.devices, sub_expires: account.sub_expires, plan: account.plan },
    )
  }, [account, form])

  const save = async () => {
    const values = await form.validateFields()
    const input = { ...values, sub_expires: values.sub_expires ?? '', plan: values.plan ?? '' }
    setSaving(true)
    try {
      if (account === 'new') await serverApi.createGeminiAccount(input)
      else if (account) await serverApi.updateGeminiAccount(account.id, { ...input, refresh_token: undefined })
      message.success('已保存')
      onSaved()
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <Modal
      title={account === 'new' ? '添加 Gemini 账号' : '编辑 Gemini 账号'}
      open={!!account}
      okText="保存"
      cancelText="取消"
      confirmLoading={saving}
      onOk={save}
      onCancel={onClose}
      destroyOnHidden
    >
      <Form form={form} layout="vertical" preserve={false}>
        <Form.Item
          name="email"
          label="Google 账号"
          extra={!isNew ? '修改账号后保存的凭据会被清除，需要重新登录。' : isJSON(refresh) ? '凭据 JSON 里有 id_token 时可以留空。' : undefined}
          rules={[{ required: !(isNew && isJSON(refresh)), type: 'email', message: '请填写邮箱' }]}
        >
          <Input placeholder="name@gmail.com" autoComplete="off" spellCheck={false} />
        </Form.Item>
        {isNew && (
          <Form.Item
            name="refresh_token"
            label="Refresh Token（可选）"
            extra="也可以粘贴 Antigravity 的完整凭据 JSON。不填则在分配到的客户端上登录后自动上报。"
          >
            <Input.TextArea rows={2} placeholder="1//0… 或 {&quot;token&quot;: …}" spellCheck={false} />
          </Form.Item>
        )}
        <PlanField kind="gemini" />
        <ExpiryField />
        <Form.Item name="note" label="备注">
          <Input maxLength={200} placeholder="如：AI Pro" />
        </Form.Item>
        <Form.Item name="max_devices" label="设备上限" rules={[{ required: true, message: '请填写设备上限' }]}>
          <InputNumber min={1} max={20} />
        </Form.Item>
        <Form.Item
          name="devices"
          label="分配到的客户端"
          dependencies={['max_devices']}
          rules={[
            ({ getFieldValue }) => ({
              validator: (_, v: number[] = []) =>
                v.length <= (getFieldValue('max_devices') ?? 0) ? Promise.resolve() : Promise.reject(new Error('超过设备上限')),
            }),
          ]}
        >
          <Select
            mode="multiple"
            allowClear
            placeholder="选择客户端"
            options={devices.filter((d) => !d.revoked).map((d) => ({ value: d.id, label: d.name }))}
          />
        </Form.Item>
      </Form>
    </Modal>
  )
}

// 各客户端当前的 Gemini 账号、建议顺序和切换（BXC-F0-023）。
export function GeminiDevicesCard({ overview, accounts, onChanged }: { overview: GeminiOverview; accounts: GeminiAccount[]; onChanged: () => void }) {
  const { message } = App.useApp()
  const email = (id: number) => accounts.find((a) => a.id === id)?.email ?? `#${id}`
  const switchTo = (d: GeminiDeviceView, accountId: number) =>
    serverApi
      .switchGemini(d.device_id, accountId)
      .then(() => message.success(`已通知「${d.name}」切换到 ${email(accountId)}`))
      .catch((e: Error) => message.error(e.message))
      .finally(onChanged)

  return (
    <Card size="small" title="Gemini 客户端与调度">
      <Table<GeminiDeviceView>
        rowKey="device_id"
        size="small"
        dataSource={overview.devices}
        pagination={false}
        scroll={{ x: 820 }}
        columns={[
          { title: '客户端', dataIndex: 'name', width: 160 },
          {
            title: '当前账号',
            dataIndex: 'active_account_id',
            width: 220,
            render: (id: number, d) => (
              <div>
                {id ? email(id) : <span className="hint">未使用</span>}
                {d.pending_switch > 0 && <div className="hint">正在切换到 {email(d.pending_switch)}</div>}
                {d.reported_at && <div className="hint">回报于 {short(d.reported_at)}</div>}
              </div>
            ),
          },
          {
            title: '建议顺序',
            dataIndex: 'order',
            render: (ids: number[]) => (
              <Space size={[4, 4]} wrap>
                {ids.length === 0 && <span className="hint">没有已登录的账号</span>}
                {ids.map((id, i) => (
                  <Tag key={id} color={i === 0 ? 'green' : undefined}>
                    {i + 1}. {email(id)}
                  </Tag>
                ))}
              </Space>
            ),
          },
          {
            title: '建议',
            dataIndex: 'suggestion',
            width: 260,
            render: (s: GeminiDeviceView['suggestion'], d) =>
              s ? (
                <Space orientation="vertical" size={4}>
                  <span className="hint">{s.reason}</span>
                  <Button size="small" type="primary" icon={<SwapOutlined />} onClick={() => switchTo(d, s.account_id)}>
                    切换到 {email(s.account_id)}
                  </Button>
                </Space>
              ) : (
                <span className="hint">—</span>
              ),
          },
          {
            title: '手动切换',
            key: 'switch',
            width: 200,
            fixed: 'right',
            render: (_, d) => (
              <Select
                size="small"
                placeholder="切换到…"
                style={{ width: 180 }}
                value={null}
                options={d.order.filter((id) => id !== d.active_account_id).map((id) => ({ value: id, label: email(id) }))}
                onChange={(id: number) => switchTo(d, id)}
              />
            ),
          },
        ]}
      />
    </Card>
  )
}

// Antigravity 的 OAuth 客户端（installed-app）：客户端 ID 默认是 Antigravity 的；密钥同样写在 Antigravity 客户端里。
export function OAuthModal({
  open,
  info,
  onClose,
  onSaved,
}: {
  open: boolean
  info?: GeminiOAuthInfo
  onClose: () => void
  onSaved: (o: GeminiOAuthInfo) => void
}) {
  const { message } = App.useApp()
  const [form] = Form.useForm<{ client_id: string; client_secret: string }>()
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    if (open) form.setFieldsValue({ client_id: info?.client_id ?? '', client_secret: '' })
  }, [open, info, form])

  const save = async () => {
    const v = await form.validateFields()
    setSaving(true)
    try {
      onSaved(await serverApi.setGeminiOAuth(v.client_id, v.client_secret))
      message.success('已保存')
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <Modal title="Antigravity OAuth 客户端" open={open} okText="保存" cancelText="取消" confirmLoading={saving} onOk={save} onCancel={onClose} destroyOnHidden width={560}>
      <Form form={form} layout="vertical" preserve={false}>
        <Form.Item name="client_id" label="客户端 ID" rules={[{ required: true, message: '请填写客户端 ID' }]}>
          <Input placeholder="xxxx.apps.googleusercontent.com" spellCheck={false} autoComplete="off" />
        </Form.Item>
        <Form.Item
          name="client_secret"
          label="客户端密钥"
          extra={info?.has_secret ? '留空表示不修改。' : undefined}
          rules={info?.has_secret ? [] : [{ required: true, message: '请填写客户端密钥' }]}
        >
          <Input.Password placeholder="GOCSPX-…" autoComplete="new-password" spellCheck={false} />
        </Form.Item>
      </Form>
    </Modal>
  )
}
