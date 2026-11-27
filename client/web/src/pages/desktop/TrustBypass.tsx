import { useCallback, useEffect, useState } from 'react'
import { Alert, App, Button, Card, Input, Modal, Switch } from 'antd'
import { UnlockOutlined } from '@ant-design/icons'
import { api, type TrustBypassStatus } from '../../api'

// 信任 Bypass（boxctl/internal/desktop/bypass.go）：打开后登录、启动、切号、额度查询不再检查保护状态，
// 保护服务在网络层的阻断照旧。打开要输入口令（防误操作，后台服务校验），关闭不需要。
// 打开期间每个页面顶部都显示红色提示（TrustBypassBanner）。

const CHANGED = 'banbu:trust-bypass'

function useTrustBypass() {
  const [st, setSt] = useState<TrustBypassStatus>()
  const load = useCallback(() => api.desktop.trustBypass().then(setSt, () => undefined), [])
  useEffect(() => {
    void load()
    const timer = setInterval(load, 5000)
    const onChange = (e: Event) => setSt((e as CustomEvent<TrustBypassStatus>).detail)
    window.addEventListener(CHANGED, onChange)
    return () => {
      clearInterval(timer)
      window.removeEventListener(CHANGED, onChange)
    }
  }, [load])
  const set = async (enabled: boolean, password?: string) => {
    const next = await api.desktop.setTrustBypass(enabled, password)
    window.dispatchEvent(new CustomEvent(CHANGED, { detail: next }))
    return next
  }
  return { st, set }
}

function sinceText(since?: string) {
  if (!since) return ''
  const d = new Date(since)
  return Number.isNaN(d.getTime()) ? '' : `（${d.toLocaleString()} 起）`
}

/** 页面顶部的红色提示，打开期间一直显示；关闭不需要口令。 */
export function TrustBypassBanner() {
  const { message } = App.useApp()
  const { st, set } = useTrustBypass()
  const [busy, setBusy] = useState(false)
  if (!st?.enabled) return null
  const off = async () => {
    setBusy(true)
    try {
      await set(false)
      message.success('已关闭信任 Bypass')
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Alert
      className="trust-bypass-banner"
      type="error"
      showIcon
      banner
      title={`信任 Bypass 已打开${sinceText(st.since)}`}
      description="登录、启动、切号和额度查询不再检查保护状态，AI 账号的流量可能不经严格线路出去。用完请立即关闭。"
      action={
        <Button size="small" danger type="primary" loading={busy} onClick={off}>
          关闭 Bypass
        </Button>
      }
    />
  )
}

/** 高级 · 保护服务里的开关。 */
export function TrustBypassCard() {
  const { message } = App.useApp()
  const { st, set } = useTrustBypass()
  const [asking, setAsking] = useState(false)
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const close = () => {
    setAsking(false)
    setPassword('')
    setError('')
  }
  const unlock = async () => {
    setBusy(true)
    try {
      await set(true, password)
      close()
      message.warning('已打开信任 Bypass，用完请关闭')
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }
  const toggle = async (on: boolean) => {
    if (on) {
      setAsking(true)
      return
    }
    setBusy(true)
    try {
      await set(false)
      message.success('已关闭信任 Bypass')
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Card
      title={
        <span className="card-title">
          <UnlockOutlined />
          信任 Bypass
        </span>
      }
      extra={<Switch checked={!!st?.enabled} loading={busy && !asking} disabled={!st} onChange={toggle} aria-label="信任 Bypass" />}
    >
      <span className="hint">
        打开后，登录、启动、切号、额度查询等账号操作不再检查保护状态（保护服务是否开启、严格线路出口是否符合）。保护服务在网络层的阻断不受影响。
        打开需要口令，关闭不需要；打开期间每个页面顶部都会显示红色提示。
      </span>
      <Modal
        open={asking}
        title="打开信任 Bypass"
        okText="打开"
        okButtonProps={{ danger: true, disabled: !password }}
        cancelText="取消"
        confirmLoading={busy}
        onOk={unlock}
        onCancel={close}
        destroyOnHidden
      >
        <p>账号操作将不再检查保护状态。请输入口令解锁：</p>
        <Input.Password
          autoFocus
          value={password}
          status={error ? 'error' : undefined}
          onChange={(e) => {
            setPassword(e.target.value)
            setError('')
          }}
          onPressEnter={() => password && void unlock()}
          placeholder="口令"
        />
        {error && <div className="ai-quota-error">{error}</div>}
      </Modal>
    </Card>
  )
}
