import { useState } from 'react'
import { Alert, Button, Form, Input } from 'antd'
import { api, type SessionInfo } from '../api'

const MIN_LEN = 10 // 与后端 auth.MinPasswordLen 一致

interface Values {
  old: string
  next: string
  confirm: string
}

export default function ChangePasswordForm({ onDone }: { onDone: (s: SessionInfo) => void }) {
  const [error, setError] = useState<string>()
  const [loading, setLoading] = useState(false)

  const submit = async (v: Values) => {
    setLoading(true)
    setError(undefined)
    try {
      onDone(await api.changePassword(v.old, v.next))
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setLoading(false)
    }
  }

  return (
    <Form<Values> layout="vertical" onFinish={submit}>
      {error && <Alert type="error" showIcon title={error} style={{ marginBottom: 16 }} />}
      <Form.Item name="old" label="当前密码" rules={[{ required: true, message: '请输入当前密码' }]}>
        <Input.Password autoComplete="current-password" />
      </Form.Item>
      <Form.Item
        name="next"
        label="新密码"
        rules={[
          { required: true, message: '请输入新密码' },
          { min: MIN_LEN, message: `至少 ${MIN_LEN} 位` },
        ]}
      >
        <Input.Password autoComplete="new-password" />
      </Form.Item>
      <Form.Item
        name="confirm"
        label="确认新密码"
        dependencies={['next']}
        rules={[
          { required: true, message: '请再次输入新密码' },
          ({ getFieldValue }) => ({
            validator: (_, value) =>
              !value || value === getFieldValue('next') ? Promise.resolve() : Promise.reject(new Error('两次输入不一致')),
          }),
        ]}
      >
        <Input.Password autoComplete="new-password" />
      </Form.Item>
      <Button type="primary" htmlType="submit" block loading={loading}>
        修改密码
      </Button>
    </Form>
  )
}
