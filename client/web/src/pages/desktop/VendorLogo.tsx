// AI 账号的厂商图标：统一用 Lobe Icons（@lobehub/icons-static-svg，MIT）的彩色版，按需打包单个 SVG。
import claudeCode from '@lobehub/icons-static-svg/icons/claudecode-color.svg'
import codex from '@lobehub/icons-static-svg/icons/codex-color.svg'
import antigravity from '@lobehub/icons-static-svg/icons/antigravity-color.svg'

export type VendorKind = 'claude' | 'codex' | 'gemini'

const icons: Record<VendorKind, { src: string; title: string }> = {
  claude: { src: claudeCode, title: 'Claude Code' },
  codex: { src: codex, title: 'Codex' },
  gemini: { src: antigravity, title: 'Antigravity' },
}

export default function VendorLogo({ kind, size = 32 }: { kind: VendorKind; size?: number }) {
  const i = icons[kind]
  return (
    <span className={`vendor-logo vendor-logo-${kind}`} style={{ width: size, height: size }} title={i.title}>
      <img src={i.src} alt={i.title} width={Math.round(size * 0.66)} height={Math.round(size * 0.66)} draggable={false} />
    </span>
  )
}
