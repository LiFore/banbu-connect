import type { ThemeConfig } from 'antd'

// 项目配色（ADR-013）。品牌色需与 desktop.css、client/app/ui/index.html 保持一致。
export const brandTheme: ThemeConfig = {
  token: {
    colorPrimary: '#6d28d9',
    colorInfo: '#6d28d9',
    colorLink: '#6d28d9',
    colorWarning: '#f59e0b',
  },
}
