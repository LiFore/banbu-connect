import type { ThemeConfig } from 'antd'
import { desktopTheme } from '../desktop/theme'

// 路由盒面板：版式和主色都与桌面客户端相同（绿色，ADR-017），不再单独用罗兰紫。
// 需与 desktop.css 的 html.box-mode 保持一致。
export const boxBrand = '#15803d'

export const boxTheme: ThemeConfig = {
  ...desktopTheme,
  token: {
    ...desktopTheme.token,
    colorWarning: '#f59e0b',
  },
}
