import { Component, type ErrorInfo, type ReactNode } from 'react'

// 页面渲染出错时显示错误，而不是整页白屏。
export default class ErrorBoundary extends Component<{ children: ReactNode }, { error?: Error }> {
  state: { error?: Error } = {}

  static getDerivedStateFromError(error: Error) {
    return { error }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error(error, info.componentStack)
  }

  render() {
    const { error } = this.state
    if (!error) return this.props.children
    return (
      <div style={{ padding: 24, fontFamily: 'system-ui, sans-serif', color: '#1c2024' }}>
        <h3>页面出错了</h3>
        <p>请把下面的内容发给开发者，然后重新打开应用。</p>
        <pre style={{ whiteSpace: 'pre-wrap', userSelect: 'text', fontSize: 12 }}>{error.stack || error.message}</pre>
        <button type="button" onClick={() => location.reload()}>
          重新加载
        </button>
      </div>
    )
  }
}
