import { useState } from 'react'
import { Alert, Button, Card, Form, Input, Typography } from 'antd'
import { api, type SessionInfo } from '../api'
import logo from '../assets/logo.png'

// server：管理服务（boxctl server）用 Banbu Connect 的品牌，提示里的路径也不同。
// box：路由盒（iStoreOS）面板。
export default function Login({ onLogin, server, box }: { onLogin: (s: SessionInfo) => void; server?: boolean; box?: boolean }) {
  const [error, setError] = useState<string>()
  const [loading, setLoading] = useState(false)

  const submit = async ({ password }: { password: string }) => {
    setLoading(true)
    setError(undefined)
    try {
      onLogin(await api.login(password))
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="login-page">
      <Card className="login-card">
        {server || box ? (
          <div className="login-brand">
            <img src={logo} alt="" />
            <div>
              <div className="login-brand-name">{box ? 'Banbu Box' : 'Banbu Connect'}</div>
              <div className="hint">{box ? '路由盒' : '管理服务'}</div>
            </div>
          </div>
        ) : (
          <Typography.Title level={3}>Boxctl 登录</Typography.Title>
        )}
        {error && <Alert type="error" showIcon title={error} style={{ marginBottom: 16 }} />}
        <Form layout="vertical" onFinish={submit}>
          <Form.Item name="password" label="管理员密码" rules={[{ required: true, message: '请输入密码' }]}>
            <Input.Password autoFocus autoComplete="current-password" />
          </Form.Item>
          <Button type="primary" htmlType="submit" block loading={loading}>
            登录
          </Button>
        </Form>
        <Typography.Paragraph type="secondary" className="login-hint">
          {server ? (
            <>
              首次使用：初始密码在数据目录的 <Typography.Text code>initial-password</Typography.Text>。忘记密码可在服务器上执行
              <Typography.Text code>boxctl reset-password -data-dir …</Typography.Text>。
            </>
          ) : (
            <>
              首次使用：初始密码在设备的 /etc/boxctl/initial-password。忘记密码可在 SSH 中执行
              <Typography.Text code>boxctl reset-password</Typography.Text>。
            </>
          )}
        </Typography.Paragraph>
      </Card>
    </div>
  )
}
