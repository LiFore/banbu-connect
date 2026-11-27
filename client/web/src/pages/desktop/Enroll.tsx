import { useCallback, useEffect, useState } from 'react'
import { Alert, App, Button, Descriptions, Form, Input, Skeleton } from 'antd'
import { KeyOutlined, LinkOutlined, ReloadOutlined, SendOutlined } from '@ant-design/icons'
import { api, type EnrollState, type ServerBinding } from '../../api'

// 接入管理服务：提交设备名称和平台，管理员在管理服务的「客户端」页面授权后，点「刷新授权」自动绑定。
// 没有管理员在线、或者已经拿到令牌时，仍可以手动填写令牌绑定。
// 引导屏和「服务端」页共用。

const time = (t?: string) => (t ? new Date(t).toLocaleString() : '—')

const STATUS_TEXT: Record<string, string> = {
  pending: '已提交申请，等待管理员授权。授权后点「刷新授权」完成绑定。',
  approved: '管理员已授权，点「刷新授权」完成绑定。',
  claimed: '授权已领取，但还没有完成绑定。',
}

export default function EnrollPanel({ onBound }: { onBound: () => void }) {
  const { message } = App.useApp()
  const [state, setState] = useState<EnrollState>()
  const [busy, setBusy] = useState(false)
  const [manual, setManual] = useState(false)
  const [manualForm] = Form.useForm<ServerBinding>()

  const load = useCallback(() => {
    api.server.enrollState().then(setState, (e: Error) => message.error(e.message))
  }, [message])

  useEffect(load, [load])

  const run = async (action: () => Promise<void>) => {
    setBusy(true)
    try {
      await action()
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const apply = (v: { url: string; name: string }) =>
    run(async () => {
      setState(await api.server.enroll(v))
      message.success('已提交申请，请联系管理员授权')
    })

  const refresh = () =>
    run(async () => {
      const s = await api.server.refreshEnrollment()
      if (s.bound) {
        message.success('已授权，配置已同步')
        onBound()
        return
      }
      setState(s)
      if (s.enrollment?.status === 'pending') message.info('管理员还没有授权')
    })

  const cancel = () => run(async () => setState(await api.server.cancelEnrollment()))

  const bind = (b: ServerBinding) =>
    run(async () => {
      await api.server.bind(b)
      message.success('已绑定，配置已同步')
      onBound()
    })

  if (!state) return <Skeleton active paragraph={{ rows: 3 }} />

  if (manual) {
    return (
      <Form form={manualForm} layout="vertical" style={{ maxWidth: 560 }} initialValues={{ url: state.default_url }} onFinish={bind}>
        <Form.Item name="url" label="管理服务地址" rules={[{ required: true, message: '请填写地址' }]}>
          <Input spellCheck={false} />
        </Form.Item>
        <Form.Item
          name="token"
          label="客户端令牌"
          extra="在管理服务的「客户端」页面新建，只显示一次。"
          rules={[{ required: true, message: '请填写令牌' }]}
        >
          <Input.Password placeholder="bc_…" spellCheck={false} />
        </Form.Item>
        <Form.Item name="fingerprint" label="证书指纹（可选）" extra="管理服务用自签名证书时必须填写。">
          <Input className="mono-input" placeholder="SHA-256，64 位十六进制" spellCheck={false} />
        </Form.Item>
        <div className="page-toolbar">
          <Button type="primary" htmlType="submit" icon={<LinkOutlined />} loading={busy}>
            绑定并同步
          </Button>
          <Button type="link" disabled={busy} onClick={() => setManual(false)}>
            改为申请接入
          </Button>
        </div>
      </Form>
    )
  }

  const e = state.enrollment
  if (e && e.status !== 'rejected') {
    return (
      <div className="desktop-stack">
        <Alert type={e.status === 'pending' ? 'info' : 'success'} showIcon title={STATUS_TEXT[e.status]} />
        {e.last_error && <Alert type="warning" showIcon title={e.last_error} />}
        <Descriptions
          column={1}
          size="small"
          items={[
            {
              label: '管理服务',
              children: <span className="selectable">{e.url}</span>,
            },
            { label: '设备名称', children: e.name },
            { label: '申请时间', children: time(e.requested_at) },
          ]}
        />
        <div className="page-toolbar">
          <Button type="primary" icon={<ReloadOutlined />} loading={busy} onClick={() => void refresh()}>
            刷新授权
          </Button>
          <Button disabled={busy} onClick={() => void cancel()}>
            取消申请
          </Button>
        </div>
      </div>
    )
  }

  return (
    <Form
      layout="vertical"
      style={{ maxWidth: 560 }}
      initialValues={{
        url: e?.url || state.default_url,
        name: e?.name || state.default_name,
      }}
      onFinish={apply}
    >
      {e?.status === 'rejected' && (
        <Alert type="error" showIcon title="申请被拒绝或已被删除，可以重新申请" description={e.last_error} style={{ marginBottom: 16 }} />
      )}
      <p className="hint">提交本机的名称和系统版本，管理员在管理服务里授权后即可绑定，不用手动输入令牌。</p>
      <Form.Item
        name="name"
        label="设备名称"
        extra={'管理员据此认出这台电脑，可以改成你的名字。'}
        rules={[{ required: true, whitespace: true, message: '请填写设备名称' }]}
      >
        <Input maxLength={64} />
      </Form.Item>
      <Form.Item name="url" label="管理服务地址" rules={[{ required: true, message: '请填写地址' }]}>
        <Input spellCheck={false} />
      </Form.Item>
      <div className="page-toolbar">
        <Button type="primary" htmlType="submit" icon={<SendOutlined />} loading={busy}>
          申请接入
        </Button>
        <Button type="link" icon={<KeyOutlined />} disabled={busy} onClick={() => setManual(true)}>
          已有令牌，手动绑定
        </Button>
      </div>
    </Form>
  )
}
