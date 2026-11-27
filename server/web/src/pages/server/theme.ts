import type { ThemeConfig } from 'antd'

// 把桌面端规范的令牌映射到 Ant Design。桌面客户端用独立的绿色（ADR-017），
// 需与 desktop.css 的 --box-brand、client/app/ui/index.html 保持一致；路由盒面板也用这个绿色。
export const desktopBrand = '#15803d'

export const desktopTheme: ThemeConfig = {
  token: {
    colorPrimary: desktopBrand,
    colorInfo: desktopBrand,
    colorInfoBg: '#f0fdf4',
    colorInfoBorder: '#bbf7d0',
    colorLink: desktopBrand,
    colorBgLayout: '#f5f6f7',
    colorBorderSecondary: '#e4e6ea',
    borderRadius: 6,
    borderRadiusLG: 8,
    fontSize: 14,
    fontFamily: '"Segoe UI", -apple-system, "PingFang SC", "Microsoft YaHei UI", "Microsoft YaHei", sans-serif',
  },
  components: {
    Card: { headerFontSize: 14, headerFontSizeSM: 14 },
    Table: { cellPaddingBlock: 8, cellPaddingInline: 12, cellPaddingBlockSM: 6 },
    Segmented: { trackPadding: 3 },
    Alert: { withDescriptionPadding: '10px 14px', withDescriptionIconSize: 16 },
  },
}
