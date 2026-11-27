import { lazy, Suspense, useState } from 'react'
import { Navigate, NavLink, Route, Routes } from 'react-router-dom'
import { Button, Modal, Skeleton } from 'antd'
import {
  ApartmentOutlined,
  BellOutlined,
  BranchesOutlined,
  ClusterOutlined,
  CloudDownloadOutlined,
  CloudUploadOutlined,
  DashboardOutlined,
  FolderOpenOutlined,
  KeyOutlined,
  LaptopOutlined,
  LinkOutlined,
  LogoutOutlined,
  ProfileOutlined,
  TeamOutlined,
  UserOutlined,
} from '@ant-design/icons'
import type { SessionInfo } from '../../api'
import logo from '../../assets/logo.png'
import ChangePasswordForm from '../ChangePassword'
import Placeholder from '../Placeholder'
import Subscriptions from '../subscriptions/Subscriptions'
import Nodes from '../nodes/Nodes'
import Groups from '../groups/Groups'
import Publish from './Publish'
import Routing from './Routing'
import Profiles from './Profiles'
import Devices from './Devices'
import Users from './Users'
import Releases from './Releases'
import AiAccounts from './AiAccounts'
import Files from './Files'
import Notify from './Notify'

// 遥测页带 ECharts，懒加载，不进管理服务其他页面和路由盒、桌面端的包（ADR-058）。
const Telemetry = lazy(() => import('./telemetry/Telemetry'))

// 导航按工作顺序：先维护订阅、节点、代理组、线路（整个系统的），再定方案（各组客户端拥有哪些、规则），然后发布；客户端和更新包放最后。
const pages = [
  { path: '/subscriptions', label: '订阅', icon: <LinkOutlined />, element: <Subscriptions /> },
  { path: '/nodes', label: '节点', icon: <ClusterOutlined />, element: <Nodes /> },
  { path: '/groups', label: '代理组', icon: <ApartmentOutlined />, element: <Groups /> },
  { path: '/routes', label: '线路', icon: <BranchesOutlined />, element: <Routing /> },
  { path: '/profiles', label: '方案', icon: <ProfileOutlined />, element: <Profiles /> },
  { path: '/publish', label: '发布', icon: <CloudUploadOutlined />, element: <Publish /> },
  { path: '/devices', label: '客户端', icon: <LaptopOutlined />, element: <Devices /> },
  { path: '/users', label: '用户', icon: <TeamOutlined />, element: <Users /> },
  {
    path: '/telemetry',
    label: '遥测',
    icon: <DashboardOutlined />,
    element: (
      <Suspense fallback={<Skeleton active paragraph={{ rows: 8 }} />}>
        <Telemetry />
      </Suspense>
    ),
  },
  { path: '/claude', label: 'AI 账号', icon: <UserOutlined />, element: <AiAccounts /> },
  { path: '/releases', label: '客户端更新', icon: <CloudDownloadOutlined />, element: <Releases /> },
  { path: '/files', label: '文件', icon: <FolderOpenOutlined />, element: <Files /> },
  { path: '/notify', label: '通知', icon: <BellOutlined />, element: <Notify /> },
]

// 管理服务（boxctl server）的外壳：沿用 Banbu Connect 客户端的顶栏导航和令牌（desktop.css），
// 但它在浏览器里打开，页面正常滚动，顶栏吸顶。
export default function ServerShell({ onSession, onLogout }: { onSession: (s: SessionInfo) => void; onLogout: () => void }) {
  const [pwdOpen, setPwdOpen] = useState(false)

  return (
    <div className="desktop-shell server-shell">
      <header className="desktop-titlebar">
        <div className="desktop-logo">
          <img src={logo} alt="" />
          <span>Banbu Connect</span>
          <span className="server-badge">管理服务</span>
        </div>
        <nav className="desktop-nav" aria-label="主导航">
          {pages.map((p) => (
            <NavLink key={p.path} to={p.path}>
              <span className="desktop-nav-icon">{p.icon}</span>
              {p.label}
            </NavLink>
          ))}
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
          <Route path="/" element={<Navigate to="/publish" replace />} />
          {/* 链式代理已并入「线路」页 */}
          <Route path="/chains/*" element={<Navigate to="/routes?tab=chains" replace />} />
          {/* Gemini 账号已并入「AI 账号」页 */}
          <Route path="/gemini/*" element={<Navigate to="/claude" replace />} />
          {pages.map((p) => (
            <Route key={p.path} path={`${p.path}/*`} element={p.element} />
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
