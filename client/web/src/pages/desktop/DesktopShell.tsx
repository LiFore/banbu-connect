import { useEffect, useState } from 'react'
import { Navigate, Route, Routes, useLocation, useNavigate } from 'react-router-dom'
import { Alert, Dropdown } from 'antd'
import { api } from '../../api'
import {
  AimOutlined,
  ApartmentOutlined,
  BarChartOutlined,
  CloudServerOutlined,
  ClusterOutlined,
  CloudDownloadOutlined,
  DeleteOutlined,
  HomeOutlined,
  LinkOutlined,
  MenuOutlined,
  NodeIndexOutlined,
  RadarChartOutlined,
  SafetyOutlined,
  SettingOutlined,
  ShrinkOutlined,
  SwapOutlined,
} from '@ant-design/icons'
import logo from '../../assets/logo.png'
import Subscriptions from '../subscriptions/Subscriptions'
import Nodes from '../nodes/Nodes'
import Groups from '../groups/Groups'
import Placeholder from '../Placeholder'
import Home from './Home'
import Proxies from './Proxies'
import Protection from './Protection'
import EgressCheck from './EgressCheck'
import BoxDiscovery from './BoxDiscovery'
import Usage from './Usage'
import Downloads from './Downloads'
import RoutePreview from './RoutePreview'
import ProxySettings from './ProxySettings'
import Settings from './Settings'
import ServerBinding from './ServerBinding'
import { OTelFooter } from './ClaudeUsage'
import Onboarding, { guideSkipped, nodeTotal } from './Onboarding'
import { WindowControls, openCompactWindow, useWindowPlatform } from './windowChrome'
import { TrustBypassBanner } from './TrustBypass'
import ForceUpdateGate from './ForceUpdate'
import Compact from './Compact'
import Uninstall from './Uninstall'

// 页面（BXC-F0-047）：首页（状态 + 账号，原来的概览和账号合在一起）、用量、下载（S1.11）；代理相关的都在「高级」。
// 顶栏不再放导航标签，只显示居中的应用名，页面切换收进角落的菜单。
// 路由盒面板不跟着改（ADR-056）。
const pages = [
  { path: '/', label: '首页', icon: <HomeOutlined />, element: <Home /> },
  { path: '/usage', label: '用量', icon: <BarChartOutlined />, element: <Usage /> },
  { path: '/downloads', label: '下载', icon: <CloudDownloadOutlined />, element: <Downloads /> },
]

// 「高级」：线路、保护服务、出口检测、管理服务绑定和本地配置。local 的页面在绑定后是本地追加的配置。
const advancedPages = [
  {
    path: '/advanced/proxies',
    label: '线路',
    icon: <SwapOutlined />,
    element: (
      <div className="desktop-stack">
        <ProxySettings />
        <Proxies />
      </div>
    ),
  },
  { path: '/advanced/protection', label: '保护服务', icon: <SafetyOutlined />, element: <Protection /> },
  { path: '/advanced/egress', label: '出口检测', icon: <AimOutlined />, element: <EgressCheck /> },
  { path: '/advanced/routes', label: '线路预览', icon: <NodeIndexOutlined />, element: <RoutePreview /> },
  { path: '/advanced/discovery', label: '路由盒发现', icon: <RadarChartOutlined />, element: <BoxDiscovery /> },
  {
    path: '/advanced/server',
    label: '管理服务',
    icon: <CloudServerOutlined />,
    element: (
      <div className="desktop-stack">
        <ServerBinding />
        <OTelFooter />
      </div>
    ),
  },
  { path: '/advanced/subscriptions', label: '订阅', icon: <LinkOutlined />, element: <Subscriptions />, local: true },
  { path: '/advanced/nodes', label: '节点', icon: <ClusterOutlined />, element: <Nodes />, local: true },
  { path: '/advanced/groups', label: '代理组', icon: <ApartmentOutlined />, element: <Groups />, local: true },
  { path: '/advanced/settings', label: '规则与设置', icon: <SettingOutlined />, element: <Settings />, local: true },
  { path: '/advanced/uninstall', label: '卸载', icon: <DeleteOutlined />, element: <Uninstall /> },
]

// 旧地址跳到新位置（保留子路径），书签和托盘打开的旧链接仍然可用。
const moved: [string, string][] = [
  ['/proxies', '/advanced/proxies'],
  ['/protection', '/advanced/protection'],
  ['/egress', '/advanced/egress'],
  ['/claude-usage', '/usage'],
  ['/accounts', '/'], // 账号页改成了首页
  ['/overview', '/'], // 概览合进了首页
]

function Moved({ from, to }: { from: string; to: string }) {
  const { pathname, search } = useLocation()
  const path = to.replace(/\/$/, '') + pathname.slice(from.length)
  return <Navigate replace to={(path || '/') + search} />
}

// 自绘标题栏：后台服务的网页只有拖动、窗口按钮和几个外壳命令的权限（见 client/README.md「安全」）。
export default function DesktopShell() {
  const [refresh, setRefresh] = useState(0)
  const location = useLocation()
  const navigate = useNavigate()

  useEffect(() => {
    document.documentElement.classList.add('desktop-mode')
    const title = document.title
    document.title = 'Banbu Connect'
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'F5') {
        e.preventDefault()
        setRefresh((n) => n + 1) // 重新挂载当前页，只刷新数据，不重载整个窗口
      }
    }
    window.addEventListener('keydown', onKey)
    return () => {
      window.removeEventListener('keydown', onKey)
      document.documentElement.classList.remove('desktop-mode')
      document.title = title
    }
  }, [])

  const platform = useWindowPlatform()

  // 第一次启动或还没有节点时先显示引导屏；读不到状态时直接进主界面，由概览页报错。
  const [guide, setGuide] = useState<boolean | undefined>(() => (guideSkipped() ? false : undefined))
  useEffect(() => {
    if (guideSkipped()) return
    Promise.all([api.desktop.settings(), api.desktop.status()]).then(
      ([settings, status]) => setGuide(!settings.onboarded || nodeTotal(status) === 0),
      () => setGuide(false),
    )
  }, [])

  // 主窗口默认显示精简界面（外壳打开 /compact），完整界面在单独的窗口里（client/app/src-tauri/src/main.rs）。
  if (location.pathname.startsWith('/compact')) return <Compact />

  // 强制更新的全屏拦截盖在引导屏和主界面之上（ForceUpdate.tsx）。
  if (guide === undefined)
    return (
      <>
        <div className="desktop-shell" data-tauri-drag-region />
        <ForceUpdateGate />
      </>
    )
  if (guide)
    return (
      <>
        <Onboarding onDone={() => setGuide(false)} />
        <ForceUpdateGate />
      </>
    )

  const allPages = [...pages, ...advancedPages]
  const here = [...allPages].reverse().find((p) => (p.path === '/' ? location.pathname === '/' : location.pathname.startsWith(p.path)))
  const openCompact = () => {
    if (!openCompactWindow()) navigate('/compact')
  }
  // 角落的两个按钮：页面菜单（首页、用量、下载和「高级」里的页面）、回到精简界面。
  const corner = (
    <div className="titlebar-actions">
      <Dropdown
        trigger={['click']}
        menu={{
          selectedKeys: here ? [here.path] : [],
          items: [
            ...pages.map((p) => ({ key: p.path, icon: p.icon, label: p.label })),
            { type: 'divider' as const },
            {
              type: 'group' as const,
              label: '高级',
              children: advancedPages.map((p) => ({ key: p.path, icon: p.icon, label: p.label })),
            },
          ],
          onClick: ({ key }) => navigate(key),
        }}
      >
        <button type="button" className="titlebar-btn" aria-haspopup="menu" title="切换页面">
          <MenuOutlined />
          <span>{here?.label ?? '页面'}</span>
        </button>
      </Dropdown>
      <button type="button" className="titlebar-btn" onClick={openCompact} title="切换到精简界面">
        <ShrinkOutlined />
        <span>精简</span>
      </button>
    </div>
  )

  return (
    <div className="desktop-shell">
      {/* 顶栏只放居中的应用名；页面切换收进角落的菜单，旁边是回到精简界面的按钮。
          macOS 的红绿灯在左，按钮放右上角；Windows 的窗口按钮在右，按钮放左上角。
          data-tauri-drag-region 只对直接点中的元素生效，空白处和 logo 都要标上。 */}
      <header className="desktop-titlebar desktop-titlebar-centered" data-platform={platform} data-tauri-drag-region>
        <div className="titlebar-side titlebar-left" data-tauri-drag-region>
          {platform !== 'mac' && corner}
        </div>
        <div className="desktop-logo" data-tauri-drag-region>
          <img src={logo} alt="" data-tauri-drag-region />
          <span data-tauri-drag-region>Banbu Connect</span>
        </div>
        <div className="titlebar-side titlebar-right" data-tauri-drag-region>
          {platform === 'mac' && corner}
          {platform === 'win' && <WindowControls />}
        </div>
      </header>
      <TrustBypassBanner />
      <main className="desktop-content" key={refresh}>
        <Routes>
          {pages.map((p) => (
            <Route key={p.path} path={p.path === '/' ? '/' : `${p.path}/*`} element={p.element} />
          ))}
          {advancedPages.map((p) => (
            <Route
              key={p.path}
              path={`${p.path}/*`}
              element={
                p.local ? (
                  <>
                    <LocalNotice />
                    {p.element}
                  </>
                ) : (
                  p.element
                )
              }
            />
          ))}
          {moved.map(([from, to]) => (
            <Route key={from} path={`${from}/*`} element={<Moved from={from} to={to} />} />
          ))}
          <Route path="*" element={<Placeholder title="页面不存在" features="" />} />
        </Routes>
      </main>
      <ForceUpdateGate />
    </div>
  )
}

// 绑定管理服务后，「高级」里的是本地追加的配置，同名时以下发的为准。
function LocalNotice() {
  const [bound, setBound] = useState(false)
  useEffect(() => {
    api.server.status().then((s) => setBound(s.bound), () => undefined)
  }, [])
  if (!bound) return null
  return (
    <Alert
      className="local-notice"
      type="info"
      showIcon
      title="已绑定管理服务：这里是本地追加的配置，与下发的合并；同名时以下发的为准。下发的内容不在这里显示。"
    />
  )
}
