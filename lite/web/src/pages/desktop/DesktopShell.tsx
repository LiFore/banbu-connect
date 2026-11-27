import { useEffect, useState } from 'react'
import { Navigate, Route, Routes, useLocation, useNavigate } from 'react-router-dom'
import { Dropdown } from 'antd'
import { BarChartOutlined, CloudDownloadOutlined, MenuOutlined, SettingOutlined, ShrinkOutlined, TeamOutlined } from '@ant-design/icons'
import logo from '../../assets/logo.png'
import Placeholder from '../Placeholder'
import Home from './Home'
import Usage from './Usage'
import Downloads from './Downloads'
import Settings from './Settings'
import { WindowControls, openCompactWindow, useWindowPlatform } from './windowChrome'
import ForceUpdateGate from './ForceUpdate'
import Compact from './Compact'

// Banbu Connect Lite 只管 AI 账号：账号（首页）、用量、下载、设置（管理服务绑定、应用更新、遥测）。
// 顶栏只显示居中的应用名，页面切换收进角落的菜单。
const pages = [
  { path: '/', label: '账号', icon: <TeamOutlined />, element: <Home /> },
  { path: '/usage', label: '用量', icon: <BarChartOutlined />, element: <Usage /> },
  { path: '/downloads', label: '下载', icon: <CloudDownloadOutlined />, element: <Downloads /> },
  { path: '/settings', label: '设置', icon: <SettingOutlined />, element: <Settings /> },
]

// 旧地址跳到新位置（保留子路径），完整版的书签、外壳打开的旧链接仍然可用。
const moved: [string, string][] = [
  ['/claude-usage', '/usage'],
  ['/accounts', '/'],
  ['/overview', '/'],
  ['/advanced/server', '/settings'],
]

function Moved({ from, to }: { from: string; to: string }) {
  const { pathname, search } = useLocation()
  const path = to.replace(/\/$/, '') + pathname.slice(from.length)
  return <Navigate replace to={(path || '/') + search} />
}

// 自绘标题栏：后台进程的网页只有拖动、窗口按钮和几个外壳命令的权限（见 client/README.md「安全」）。
export default function DesktopShell() {
  const [refresh, setRefresh] = useState(0)
  const location = useLocation()
  const navigate = useNavigate()

  useEffect(() => {
    document.documentElement.classList.add('desktop-mode')
    const title = document.title
    document.title = 'Banbu Connect Lite'
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

  // 主窗口默认显示精简界面（外壳打开 /compact），完整界面在单独的窗口里（client/app/src-tauri/src/main.rs）。
  if (location.pathname.startsWith('/compact')) return <Compact />

  const here = [...pages].reverse().find((p) => (p.path === '/' ? location.pathname === '/' : location.pathname.startsWith(p.path)))
  const openCompact = () => {
    if (!openCompactWindow()) navigate('/compact')
  }
  // 角落的两个按钮：页面菜单、回到精简界面。
  const corner = (
    <div className="titlebar-actions">
      <Dropdown
        trigger={['click']}
        menu={{
          selectedKeys: here ? [here.path] : [],
          items: pages.map((p) => ({ key: p.path, icon: p.icon, label: p.label })),
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
      {/* macOS 的红绿灯在左，按钮放右上角；Windows 的窗口按钮在右，按钮放左上角。
          data-tauri-drag-region 只对直接点中的元素生效，空白处和 logo 都要标上。 */}
      <header className="desktop-titlebar desktop-titlebar-centered" data-platform={platform} data-tauri-drag-region>
        <div className="titlebar-side titlebar-left" data-tauri-drag-region>
          {platform !== 'mac' && corner}
        </div>
        <div className="desktop-logo" data-tauri-drag-region>
          <img src={logo} alt="" data-tauri-drag-region />
          <span data-tauri-drag-region>Banbu Connect Lite</span>
        </div>
        <div className="titlebar-side titlebar-right" data-tauri-drag-region>
          {platform === 'mac' && corner}
          {platform === 'win' && <WindowControls />}
        </div>
      </header>
      <main className="desktop-content" key={refresh}>
        <Routes>
          {pages.map((p) => (
            <Route key={p.path} path={p.path === '/' ? '/' : `${p.path}/*`} element={p.element} />
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
