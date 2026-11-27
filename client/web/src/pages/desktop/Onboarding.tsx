import { useCallback, useEffect, useState } from 'react'
import { App, Button, Segmented, Steps, Switch } from 'antd'
import { ApiOutlined, CheckCircleFilled, ClusterOutlined, CloudServerOutlined, LinkOutlined, SafetyOutlined } from '@ant-design/icons'
import { useNavigate } from 'react-router-dom'
import { api, ApiRequestError, type DesktopSummary } from '../../api'
import EnrollPanel from './Enroll'
import logo from '../../assets/logo.png'
import { WindowControls, useWindowPlatform } from './windowChrome'

// 引导屏：第一次启动（settings.onboarded 为 false）或还没有任何节点时显示。
// 后台服务已经装好才能打开这个页面，所以第一步总是完成的；没装服务时由外壳的本地安装页引导（client/app/ui/index.html）。

const SKIP_KEY = 'banbu_guide_skipped'

/** 「稍后再说」只在本次窗口里生效，下次启动设置还没完成时仍会显示。 */
export function guideSkipped(): boolean {
  try {
    return sessionStorage.getItem(SKIP_KEY) === '1'
  } catch {
    return false
  }
}

function skipForSession() {
  try {
    sessionStorage.setItem(SKIP_KEY, '1')
  } catch {
    // 存储不可用时只关掉这一次
  }
}

export function nodeTotal(s: DesktopSummary): number {
  return s.core.node_count + (s.server?.bound ? s.server.nodes : 0)
}

const wait = (ms: number) => new Promise((r) => setTimeout(r, ms))

export default function Onboarding({ onDone }: { onDone: () => void }) {
  const { message } = App.useApp()
  const navigate = useNavigate()
  const platform = useWindowPlatform()
  const [status, setStatus] = useState<DesktopSummary>()
  const [step, setStep] = useState(1)
  const [source, setSource] = useState<'server' | 'local'>('server')
  const [busy, setBusy] = useState(false)
  const [tun, setTun] = useState(true)
  const [killSwitch, setKillSwitch] = useState(true)

  const load = useCallback(() => {
    api.desktop.status().then(setStatus, () => undefined)
  }, [])

  useEffect(() => {
    load()
    api.desktop.settings().then((s) => setTun(s.tun), () => undefined)
    const timer = setInterval(load, 3000)
    return () => clearInterval(timer)
  }, [load])

  const nodes = status ? nodeTotal(status) : 0

  const skip = (to?: string) => {
    skipForSession()
    onDone()
    if (to) navigate(to)
  }

  // 内核刚启动时 Kill Switch 会报 not_ready，等内核起来再试；一直不行就留给概览页处理。
  const enableKillSwitch = async () => {
    for (let i = 0; ; i++) {
      try {
        await api.killswitch.enable(false)
        return
      } catch (e) {
        if (!(e instanceof ApiRequestError && e.body.code === 'not_ready') || i >= 10) throw e
        await wait(1500)
      }
    }
  }

  const finish = async () => {
    setBusy(true)
    try {
      await api.desktop.updateSettings({ tun, onboarded: true })
      await api.desktop.start()
      if (killSwitch) {
        try {
          await enableKillSwitch()
        } catch (e) {
          message.warning(`保护服务没有启用：${(e as Error).message} 可以稍后在概览页打开。`)
        }
      }
      onDone()
      navigate('/')
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="desktop-shell onboarding">
      <header className="desktop-titlebar onboarding-titlebar" data-platform={platform} data-tauri-drag-region>
        <div className="desktop-titlebar-fill" data-tauri-drag-region />
        {platform === 'win' && <WindowControls />}
      </header>
      <main className="onboarding-body">
        <div className="onboarding-panel">
          <div className="onboarding-hero">
            <img src={logo} alt="" />
            <h1>欢迎使用 Banbu Connect</h1>
            <p>三步完成设置，之后在菜单栏或托盘里就能开关代理。</p>
          </div>

          <Steps
            size="small"
            current={step}
            className="onboarding-steps"
            items={[{ title: '安装后台服务' }, { title: '获取线路' }, { title: '开启保护' }]}
          />

          {step === 1 && (
            <section className="onboarding-card">
              {nodes > 0 ? (
                <div className="onboarding-done">
                  <CheckCircleFilled />
                  <div>
                    <div className="onboarding-done-title">已有 {nodes} 个节点</div>
                    <div className="hint">
                      {status?.server?.bound ? `来自管理服务 ${status.server.url}` : '来自本地的订阅和节点'}
                    </div>
                  </div>
                </div>
              ) : (
                <>
                  <Segmented<'server' | 'local'>
                    block
                    value={source}
                    onChange={setSource}
                    options={[
                      { value: 'server', label: '绑定管理服务', icon: <CloudServerOutlined /> },
                      { value: 'local', label: '自己导入', icon: <ClusterOutlined /> },
                    ]}
                  />
                  {source === 'server' ? (
                    <div className="onboarding-form">
                      <p className="hint">节点、代理组、线路和规则由管理服务统一下发，之后自动同步；应用更新也从这里获取。</p>
                      <EnrollPanel
                        onBound={() => {
                          load()
                          setStep(2)
                        }}
                      />
                    </div>
                  ) : (
                    <div className="onboarding-local">
                      <p className="hint">没有管理服务时，可以导入机场订阅，或手动添加节点。导入完回到概览即可开启代理。</p>
                      <div className="page-toolbar">
                        <Button icon={<LinkOutlined />} onClick={() => skip('/advanced/subscriptions')}>
                          导入订阅
                        </Button>
                        <Button icon={<ClusterOutlined />} onClick={() => skip('/advanced/nodes')}>
                          手动添加节点
                        </Button>
                      </div>
                    </div>
                  )}
                </>
              )}
            </section>
          )}

          {step === 2 && (
            <section className="onboarding-card">
              <div className="setting-row">
                <span className="onboarding-option">
                  <ApiOutlined />
                  <span>
                    <span className="onboarding-option-title">接管全部流量（TUN）</span>
                    <span className="hint">不支持系统代理的程序也经过代理。需要时可在概览页关闭。</span>
                  </span>
                </span>
                <Switch aria-label="接管全部流量" checked={tun} onChange={setTun} />
              </div>
              <div className="setting-row">
                <span className="onboarding-option">
                  <SafetyOutlined />
                  <span>
                    <span className="onboarding-option-title">保护服务（推荐）</span>
                    <span className="hint">代理出故障或网络变化时，Claude 等受保护的服务直接断开，不会悄悄直连。</span>
                  </span>
                </span>
                <Switch aria-label="保护服务" checked={killSwitch} onChange={setKillSwitch} />
              </div>
            </section>
          )}

          <div className="onboarding-actions">
            <Button type="text" disabled={busy} onClick={() => skip()}>
              稍后再说
            </Button>
            <span className="desktop-titlebar-fill" />
            {step === 2 && (
              <Button disabled={busy} onClick={() => setStep(1)}>
                上一步
              </Button>
            )}
            {step === 1 &&
              (nodes > 0 ? (
                <Button type="primary" onClick={() => setStep(2)}>
                  下一步
                </Button>
              ) : null)}
            {step === 2 && (
              <Button type="primary" loading={busy} onClick={() => void finish()}>
                开始使用
              </Button>
            )}
          </div>
        </div>
      </main>
    </div>
  )
}
