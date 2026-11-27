import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import { ConfigProvider } from 'antd'
import zhCN from 'antd/locale/zh_CN'
import App from './App'
import { brandTheme } from './theme'
import './common.css'
import './desktop.css'

// 管理服务的后台页面在 /admin 下（根路径是服务端渲染的落地页，见 boxctl/internal/panel/landing.go）；
// 路由盒和桌面客户端没有前缀。
const basename = location.pathname === '/admin' || location.pathname.startsWith('/admin/') ? '/admin' : undefined

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ConfigProvider locale={zhCN} theme={brandTheme}>
      <BrowserRouter basename={basename}>
        <App />
      </BrowserRouter>
    </ConfigProvider>
  </StrictMode>,
)
