import { useEffect, useState } from 'react'
import { NavLink, Route, Routes, useLocation, useNavigate } from 'react-router-dom'
import { Alert, Button, Dropdown, Modal } from 'antd'
import {
  ApartmentOutlined,
  DesktopOutlined,
  FileTextOutlined,
  LineChartOutlined,
  CloudServerOutlined,
  ClusterOutlined,
  DashboardOutlined,
  DownOutlined,
  HistoryOutlined,
  KeyOutlined,
  LinkOutlined,
  LogoutOutlined,
  SafetyOutlined,
  SettingOutlined,
  SwapOutlined,
  ToolOutlined,
} from '@ant-design/icons'
import { api, type SessionInfo } from '../../api'
import logo from '../../assets/logo.png'
import ChangePasswordForm from '../ChangePassword'
import Placeholder from '../Placeholder'
import Subscriptions from '../subscriptions/Subscriptions'
import Nodes from '../nodes/Nodes'
import Groups from '../groups/Groups'
import Proxies from '../desktop/Proxies'
import Settings from '../desktop/Settings'
import ServerBinding from '../desktop/ServerBinding'
import BoxHome from './BoxHome'
import BoxProtection from './BoxProtection'
import Versions from './Versions'
import Devices from './Devices'
import Connections from './Connections'
import Logs from './Logs'

const pages = [
  { path: '/', label: '概览', icon: <DashboardOutlined />, element: <BoxHome /> },
  { path: '/proxies', label: '线路', icon: <SwapOutlined />, element: <Proxies /> },
  { path: '/devices', label: '设备', icon: <DesktopOutlined />, element: <Devices /> },
  { path: '/connections', label: '连接', icon: <LineChartOutlined />, element: <Connections /> },
  { path: '/protection', label: '保护服务', icon: <SafetyOutlined />, element: <BoxProtection /> },
]

// 「高级」与桌面客户端相同：绑定后以下发的为准，这里的是本地追加。
const advancedPages = [
  { path: '/advanced/server', label: '管理服务', icon: <CloudServerOutlined />, element: <ServerBinding /> },
  { path: '/advanced/subscriptions', label: '订阅', icon: <LinkOutlined />, element: <Subscriptions /> },
  { path: '/advanced/nodes', label: '节点', icon: <ClusterOutlined />, element: <Nodes /> },
  { path: '/advanced/groups', label: '代理组', icon: <ApartmentOutlined />, element: <Groups /> },
  { path: '/advanced/settings', label: '规则与设置', icon: <SettingOutlined />, element: <Settings /> },
  { path: '/advanced/versions', label: '配置版本', icon: <HistoryOutlined />, element: <Versions /> },
  { path: '/advanced/logs', label: '日志', icon: <FileTextOutlined />, element: <Logs /> },
]

// 路由盒（iStoreOS）面板的外壳：版式与 Banbu Connect 客户端一致（顶栏导航 + 卡片），
// 在浏览器里打开，沿用管理服务的吸顶顶栏；主色按 ADR-013 用罗兰紫。
export default function BoxShell({ onSession, onLogout }: { onSession: (s: SessionInfo) => void; onLogout: () => void }) {
  const location = useLocation()
  const navigate = useNavigate()
  const [pwdOpen, setPwdOpen] = useState(false)
  const current = advancedPages.find((p) => location.pathname.startsWith(p.path))

  return (
    <div className="desktop-shell server-shell">
      <header className="desktop-titlebar">
        <div className="desktop-logo">
          <img src={logo} alt="" />
          <span>Banbu Box</span>
          <span className="server-badge">路由盒</span>
        </div>
        <nav className="desktop-nav" aria-label="主导航">
          {pages.map((p) => (
            <NavLink key={p.path} to={p.path} end={p.path === '/'}>
              <span className="desktop-nav-icon">{p.icon}</span>
              {p.label}
            </NavLink>
          ))}
          <Dropdown
            trigger={['click']}
            menu={{
              selectedKeys: current ? [current.path] : [],
              items: advancedPages.map((p) => ({ key: p.path, icon: p.icon, label: p.label })),
              onClick: ({ key }) => navigate(key),
            }}
          >
            <a className={current ? 'active' : undefined} role="button" aria-haspopup="menu">
              <span className="desktop-nav-icon">
                <ToolOutlined />
              </span>
              {current ? `高级 · ${current.label}` : '高级'}
              <DownOutlined className="desktop-nav-caret" />
            </a>
          </Dropdown>
        </nav>
        <div className="desktop-titlebar-fill" />
        <div className="server-actions">
          <Button type="text" size="small" icon={<KeyOutlined />} onClick={() => setPwdOpen(true)}>
            修改密码
          </Button>
          <Button type="text" size="small" icon={<LogoutOutlined />} onClick={onLogout}>
            退出
          </Button>
        </div>
      </header>
      <main className="desktop-content server-content">
        <Routes>
          {pages.map((p) => (
            <Route key={p.path} path={p.path === '/' ? '/' : `${p.path}/*`} element={p.element} />
          ))}
          {advancedPages.map((p) => (
            <Route
              key={p.path}
              path={`${p.path}/*`}
              element={
                ['/advanced/server', '/advanced/versions', '/advanced/logs'].includes(p.path) ? (
                  p.element
                ) : (
                  <>
                    <LocalNotice />
                    {p.element}
                  </>
                )
              }
            />
          ))}
          <Route path="*" element={<Placeholder title="页面不存在" features="" />} />
        </Routes>
      </main>
      <Modal title="修改密码" open={pwdOpen} onCancel={() => setPwdOpen(false)} footer={null} destroyOnHidden>
        <ChangePasswordForm
          onDone={(s) => {
            setPwdOpen(false)
            onSession(s)
          }}
        />
      </Modal>
    </div>
  )
}

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
