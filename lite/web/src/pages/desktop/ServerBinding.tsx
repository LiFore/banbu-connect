import { useCallback, useEffect, useState } from 'react'
import { Alert, App, Button, Card, Descriptions, Skeleton } from 'antd'
import { CloudServerOutlined, DisconnectOutlined, SyncOutlined } from '@ant-design/icons'
import { api, type ServerStatus } from '../../api'
import EnrollPanel from './Enroll'

const time = (t?: string) => (t ? new Date(t).toLocaleString() : '—')

// 绑定管理服务：托管的 AI 账号（Claude、Codex、Antigravity）、额度和应用更新都来自管理服务。
export default function ServerBinding() {
  const { message, modal } = App.useApp()
  const [status, setStatus] = useState<ServerStatus>()
  const [busy, setBusy] = useState(false)

  const load = useCallback(() => {
    api.server.status().then(setStatus, (e: Error) => message.error(e.message))
  }, [message])

  useEffect(() => {
    load()
    const timer = setInterval(load, 10000)
    return () => clearInterval(timer)
  }, [load])

  const run = async (action: () => Promise<ServerStatus>, done: string) => {
    setBusy(true)
    try {
      setStatus(await action())
      message.success(done)
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  if (!status) {
    return (
      <Card>
        <Skeleton active paragraph={{ rows: 4 }} />
      </Card>
    )
  }

  if (!status.bound) {
    return (
      <Card
        title={
          <span className="card-title">
            <CloudServerOutlined />
            绑定管理服务
          </span>
        }
      >
        <p className="hint">
          绑定后，管理员分配给这台电脑的 AI 账号会出现在「账号」页，每 5 分钟同步一次；应用更新也从管理服务获取。
        </p>
        <EnrollPanel onBound={load} />
      </Card>
    )
  }

  return (
    <div className="desktop-stack">
      {status.last_error && <Alert type="warning" showIcon title="最近一次同步失败" description={status.last_error} />}
      <Card
        title={
          <span className="card-title">
            <CloudServerOutlined />
            管理服务
          </span>
        }
        extra={
          <div className="page-toolbar">
            <Button icon={<SyncOutlined />} loading={busy} onClick={() => run(api.server.sync, '已同步')}>
              立即同步
            </Button>
            <Button
              danger
              icon={<DisconnectOutlined />}
              disabled={busy}
              onClick={() =>
                modal.confirm({
                  title: '解除绑定？',
                  content: '托管的账号不再同步到这台电脑，之后也不再收到应用更新。',
                  okText: '解除绑定',
                  okButtonProps: { danger: true },
                  cancelText: '取消',
                  onOk: () => run(api.server.unbind, '已解除绑定'),
                })
              }
            >
              解除绑定
            </Button>
          </div>
        }
      >
        <Descriptions
          column={2}
          size="small"
          items={[
            { label: '地址', children: <span className="selectable">{status.url}</span> },
            { label: '证书', children: status.pinned ? '按指纹校验' : '系统证书' },
            { label: '配置版本', children: status.version ? `第 ${status.version} 版` : '—' },
            { label: '发布时间', children: time(status.published_at) },
            { label: '说明', children: status.note || '—' },
            { label: '最近同步', children: time(status.last_sync) },
          ]}
        />
      </Card>
    </div>
  )
}
