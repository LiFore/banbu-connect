import { useCallback, useEffect, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { Alert, App, Button, Switch } from 'antd'
import { ExpandOutlined, PoweroffOutlined } from '@ant-design/icons'
import { api, type DesktopSummary, type ProxyMode, type TrayStats } from '../../api'
import logo from '../../assets/logo.png'
import { ksStates, overallState } from './Home'
import CountryFlag from './CountryFlag'
import { WindowControls, inShell, openFullWindow, quitApp, useWindowPlatform } from './windowChrome'
import ForceUpdateGate from './ForceUpdate'
import Accounts from './Accounts'
import QuitConfirm from './QuitConfirm'

// 精简主界面：主窗口默认显示它（外壳打开 /compact），和完整界面是两个页面。
// 自上而下：代理一张卡片（总体状态 + 开关，下面两行出口、模式、保护和实时速率；最近一分钟的速率曲线淡淡地铺在卡片背景上），
// 然后是账号（和完整界面首页同一套卡片，一排横向滚动），底部是「完整界面」和醒目的「退出」。
// 代理的详细信息（网络、节点、连接）都在完整界面。

const HISTORY = 60 // 速率曲线保留最近 60 个点（每 2 秒一个，约 2 分钟）

const modeLabels: Record<ProxyMode, string> = { rule: '规则', global: '全局', direct: '直连' }

export default function Compact() {
  const platform = useWindowPlatform()
  const navigate = useNavigate()
  const { message } = App.useApp()
  // 托盘的「退出」也在这里确认（ADR-090）：外壳带 ?quit=1 打开精简页，或已在精简页时发 banbu-quit 事件。
  const [search, setSearch] = useSearchParams()
  const [quitOpen, setQuitOpen] = useState(() => search.get('quit') === '1')
  useEffect(() => {
    if (search.has('quit')) setSearch((p) => (p.delete('quit'), p), { replace: true })
  }, [search, setSearch])
  useEffect(() => {
    const ask = () => setQuitOpen(true)
    window.addEventListener('banbu-quit', ask)
    return () => window.removeEventListener('banbu-quit', ask)
  }, [])
  const [status, setStatus] = useState<DesktopSummary>()
  const [stats, setStats] = useState<TrayStats>()
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  const [history, setHistory] = useState<{ up: number[]; down: number[] }>({ up: [], down: [] })

  const load = useCallback(() => {
    api.desktop.status().then(
      (s) => {
        setStatus(s)
        setError(undefined)
      },
      (e: Error) => setError(e.message),
    )
  }, [])

  // 速率 2 秒取一次，状态 3 秒一次；窗口藏到后台时都停下。
  useEffect(() => {
    const tick = () =>
      api.desktop.trayStats().then(
        (st) => {
          if (st.running) setHistory((h) => ({ up: [...h.up, st.up].slice(-HISTORY), down: [...h.down, st.down].slice(-HISTORY) }))
          setStats(st)
        },
        () => undefined,
      )
    let fast: ReturnType<typeof setInterval> | undefined
    let slow: ReturnType<typeof setInterval> | undefined
    const start = () => {
      if (fast) return
      load()
      void tick()
      fast = setInterval(tick, 2000)
      slow = setInterval(load, 3000)
    }
    const stop = () => {
      clearInterval(fast)
      clearInterval(slow)
      fast = slow = undefined
    }
    const onVis = () => (document.visibilityState === 'visible' ? start() : stop())
    start()
    document.addEventListener('visibilitychange', onVis)
    return () => {
      stop()
      document.removeEventListener('visibilitychange', onVis)
    }
  }, [load])

  const toggleProxy = async (on: boolean) => {
    setBusy(true)
    try {
      await (on ? api.desktop.start() : api.desktop.stop())
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setBusy(false)
      load()
    }
  }

  // 没有外壳（在浏览器里预览）时在当前页跳到完整界面。
  const openFull = () => {
    if (!openFullWindow()) navigate('/')
  }

  const quit = () => {
    setQuitOpen(false)
    if (!quitApp(true)) message.info('请在桌面客户端里退出')
  }

  const summary = status ? overallState(status) : undefined
  const nodes = status ? status.core.node_count + (status.server?.bound ? status.server.nodes : 0) : 1
  const ks = status?.killswitch
  const eg = status?.network.egress

  return (
    <div className="desktop-shell compact-shell">
      <header className="desktop-titlebar desktop-titlebar-centered" data-platform={platform} data-tauri-drag-region>
        <div className="titlebar-side" data-tauri-drag-region />
        <div className="desktop-logo" data-tauri-drag-region>
          <img src={logo} alt="" data-tauri-drag-region />
          <span data-tauri-drag-region>Banbu Connect</span>
        </div>
        <div className="titlebar-side titlebar-right" data-tauri-drag-region>
          {platform === 'win' && <WindowControls />}
        </div>
      </header>

      <main className="compact-body">
        {error && <Alert type="error" showIcon title="连不上后台服务" description={error} />}
        {status && nodes === 0 && <Alert type="info" showIcon title="还没有节点，到完整界面添加" />}

        <section className="compact-proxy" id="overview-status" aria-label="代理">
          {stats?.running && <Spark up={history.up} down={history.down} />}
          {/* 高度固定：标题一行、说明两行（不够也占位）、开关和要点两行始终都在，加载前后、文字长短不同时卡片不跳 */}
          <div className="compact-status" data-tone={summary?.tone ?? 'off'} aria-live="polite">
            <span className="compact-dot" aria-hidden />
            <div className="compact-status-text">
              <div className="compact-title" title={summary?.title}>
                {summary?.title ?? '正在连接后台服务…'}
              </div>
              <div className="compact-desc selectable" title={summary?.desc}>
                {summary?.desc ?? (status ? '' : '正在读取代理状态')}
              </div>
            </div>
            <Switch
              aria-label="代理开关"
              checked={status?.core.desired ?? false}
              disabled={!status}
              loading={busy || !!status?.core.applying}
              onChange={(on) => void toggleProxy(on)}
            />
          </div>
          <div className="compact-meta">
            <span className="selectable" title={eg?.error ? `出口探测失败：${eg.error}` : eg?.ip ? `出口 ${eg.ip}` : '出口'}>
              {status?.network.probing && eg?.ok ? (
                <>
                  <CountryFlag code={eg.country} />
                  {eg.ip}
                </>
              ) : status?.network.probing ? (
                eg?.error ? '出口探测失败' : '出口探测中'
              ) : (
                '出口 —'
              )}
            </span>
            <span className="compact-meta-end">
              {status ? (
                <>
                  {modeLabels[status.core.mode]}
                  {status.core.tun ? ' · TUN' : ''}
                  {status.network.box?.id ? ' · 路由盒' : ''}
                </>
              ) : (
                '模式 —'
              )}
            </span>
            <span data-tone={ks?.enabled ? ksStates[ks.state].tone : 'off'} title={ks?.enabled ? `保护：${ksStates[ks.state].label}` : '保护'}>
              {!status ? '保护 —' : ks?.enabled ? ksStates[ks.state].label : '保护未启用'}
            </span>
            <span
              className="compact-meta-end compact-meta-rate"
              title={stats?.running ? `${stats.connections} 连接，其中 ${stats.proxied} 个经代理` : undefined}
            >
              ↓ {bytes(stats?.running ? stats.down : 0)}/s ↑ {bytes(stats?.running ? stats.up : 0)}/s
            </span>
          </div>
        </section>

        <Accounts compact />
      </main>

      <footer className="compact-footer">
        <Button icon={<ExpandOutlined />} onClick={openFull}>
          完整界面
        </Button>
        {inShell() && (
          <Button type="primary" danger icon={<PoweroffOutlined />} onClick={() => setQuitOpen(true)}>
            退出
          </Button>
        )}
      </footer>
      {quitOpen && <QuitConfirm onCancel={() => setQuitOpen(false)} onQuit={quit} />}
      <ForceUpdateGate />
    </div>
  )
}

// 速率曲线：铺满代理卡片背景，下载为浅色填充、上传为细线，纵轴按两条线里的最大值缩放。
function Spark({ up, down }: { up: number[]; down: number[] }) {
  const max = Math.max(1024, ...up, ...down)
  const pts = (arr: number[]) => {
    const step = 300 / (HISTORY - 1)
    const off = (HISTORY - arr.length) * step
    return arr.map((v, i) => [off + i * step, 60 - (v / max) * 52] as const)
  }
  const line = (arr: number[]) =>
    arr.length < 2 ? '' : pts(arr).map(([x, y], i) => `${i ? 'L' : 'M'}${x.toFixed(1)},${y.toFixed(1)}`).join('')
  const area = (arr: number[]) => {
    if (arr.length < 2) return ''
    const p = pts(arr)
    return `${line(arr)}L${p[p.length - 1][0].toFixed(1)},60L${p[0][0].toFixed(1)},60Z`
  }
  return (
    <svg className="compact-spark" viewBox="0 0 300 60" preserveAspectRatio="none" aria-hidden>
      <path d={area(down)} fill="var(--box-down)" fillOpacity="0.1" stroke="none" />
      <path d={line(down)} fill="none" stroke="var(--box-down)" strokeOpacity="0.45" strokeWidth="1.25" vectorEffect="non-scaling-stroke" />
      <path d={line(up)} fill="none" stroke="var(--box-up)" strokeOpacity="0.45" strokeWidth="1.25" vectorEffect="non-scaling-stroke" />
    </svg>
  )
}

function bytes(n: number): string {
  if (!n || n < 0) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  while (n >= 1024 && i < u.length - 1) {
    n /= 1024
    i++
  }
  return `${i === 0 ? n.toFixed(0) : n.toFixed(n < 10 ? 2 : n < 100 ? 1 : 0)} ${u[i]}`
}
