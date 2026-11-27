import { useMemo } from 'react'
import DOMPurify from 'dompurify'
import { marked } from 'marked'

// Markdown 渲染：marked 转 HTML，DOMPurify 清理（说明和文件由管理员上传，仍按不可信内容处理）。
// 链接一律在新窗口打开。只经 ./Markdown 懒加载，不进其他页面的包。

DOMPurify.addHook('afterSanitizeAttributes', (node) => {
  if (node.tagName === 'A') {
    node.setAttribute('target', '_blank')
    node.setAttribute('rel', 'noopener noreferrer')
  }
})

export default function MarkdownView({ source, compact }: { source: string; compact?: boolean }) {
  const html = useMemo(() => {
    const raw = marked.parse(source, { gfm: true, breaks: true, async: false })
    return DOMPurify.sanitize(raw, { FORBID_TAGS: ['style', 'form', 'input', 'button'], FORBID_ATTR: ['style'] })
  }, [source])
  return <div className={`markdown-body${compact ? ' compact' : ''}`} dangerouslySetInnerHTML={{ __html: html }} />
}
