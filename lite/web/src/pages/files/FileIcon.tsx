import {
  FileExcelOutlined,
  FileImageOutlined,
  FileMarkdownOutlined,
  FileOutlined,
  FilePdfOutlined,
  FilePptOutlined,
  FileTextOutlined,
  FileWordOutlined,
  FileZipOutlined,
  VideoCameraOutlined,
  WindowsOutlined,
  AppleOutlined,
} from '@ant-design/icons'
import type { ReactNode } from 'react'

// 按扩展名显示文件图标（云盘文件，管理端和客户端共用）。
const kinds: [RegExp, ReactNode, string][] = [
  [/\.(md|markdown)$/i, <FileMarkdownOutlined />, 'md'],
  [/\.pdf$/i, <FilePdfOutlined />, 'pdf'],
  [/\.(zip|rar|7z|tar|gz|tgz|xz|bz2)$/i, <FileZipOutlined />, 'zip'],
  [/\.(dmg|pkg)$/i, <AppleOutlined />, 'app'],
  [/\.(exe|msi|msix)$/i, <WindowsOutlined />, 'app'],
  [/\.(png|jpe?g|gif|webp|svg|heic)$/i, <FileImageOutlined />, 'img'],
  [/\.(mp4|mov|mkv|avi|webm)$/i, <VideoCameraOutlined />, 'video'],
  [/\.(docx?|pages)$/i, <FileWordOutlined />, 'doc'],
  [/\.(xlsx?|csv|numbers)$/i, <FileExcelOutlined />, 'sheet'],
  [/\.(pptx?|key)$/i, <FilePptOutlined />, 'slides'],
  [/\.(txt|log|json|ya?ml|conf|ini)$/i, <FileTextOutlined />, 'text'],
]

export default function FileIcon({ name }: { name: string }) {
  const k = kinds.find(([re]) => re.test(name))
  return (
    <span className="file-icon" data-kind={k?.[2] ?? 'file'} aria-hidden>
      {k?.[1] ?? <FileOutlined />}
    </span>
  )
}
