import { lazy, Suspense } from 'react'
import { Skeleton } from 'antd'

const MarkdownView = lazy(() => import('./MarkdownView'))

// Markdown 显示（云盘文件的说明、Markdown 文件）。渲染库懒加载，只在用到时下载。
export default function Markdown({ source, compact }: { source: string; compact?: boolean }) {
  if (!source.trim()) return null
  return (
    <Suspense fallback={<Skeleton active title={false} paragraph={{ rows: compact ? 1 : 6 }} />}>
      <MarkdownView source={source} compact={compact} />
    </Suspense>
  )
}
