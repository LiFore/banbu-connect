import { BorderOutlined, CloseOutlined, MinusOutlined } from '@ant-design/icons'
import { useEffect, useState } from 'react'
import { desktopShell } from '../../api'

// 自绘标题栏：系统标题栏去掉后（macOS 叠加红绿灯，Windows 无边框），顶栏就是标题栏。
// 后台服务的网页只有拖动、最小化、最大化、关闭这几个 Tauri 权限（client/app/src-tauri/capabilities/titlebar.json）。

interface TauriWindow {
  minimize(): Promise<void>
  toggleMaximize(): Promise<void>
  close(): Promise<void>
}

declare global {
  interface Window {
    __TAURI__?: {
      window?: { getCurrentWindow?: () => TauriWindow }
      core?: { invoke?: (cmd: string, args?: Record<string, unknown>) => Promise<unknown> }
    }
  }
}

// 外壳命令：后台服务的网页只能调用这几个（client/app/src-tauri/capabilities/titlebar.json）。
// 在浏览器里打开（预览）时没有外壳，返回 false。
function shellInvoke(cmd: string, args?: Record<string, unknown>): boolean {
  const invoke = window.__TAURI__?.core?.invoke
  if (!invoke) return false
  void invoke(cmd, args)
  return true
}

/** 打开完整界面窗口（原来的主界面），path 是要打开的页面（默认首页）。没有外壳时返回 false，由调用方在当前页跳过去。 */
export function openFullWindow(path?: string): boolean {
  return shellInvoke('shell_open_full', path ? { path } : undefined)
}

/** 回到主窗口的精简界面并关掉完整界面窗口。没有外壳时返回 false，由调用方在当前页跳过去。 */
export function openCompactWindow(): boolean {
  return shellInvoke('shell_open_compact')
}

/** 退出应用（外壳和它拉起的后台进程一起退出）。没有外壳时返回 false。 */
export function quitApp(): boolean {
  return shellInvoke('shell_quit')
}

/** 是否在桌面外壳里（能调用外壳命令）。 */
export function inShell(): boolean {
  return !!window.__TAURI__?.core?.invoke
}

export type Platform = 'mac' | 'win' | 'web'

function currentWindow(): TauriWindow | undefined {
  return window.__TAURI__?.window?.getCurrentWindow?.()
}

// 外壳在页面加载后设置 html[data-shell] 并发出 banbu-shell 事件（也会在地址里带 shell=）；
// 在浏览器里打开（预览）时没有，保持普通网页的样子。
function readPlatform(): Platform {
  const v = document.documentElement.dataset.shell || desktopShell
  return v === 'mac' || v === 'win' ? v : 'web'
}

export function useWindowPlatform(): Platform {
  const [platform, setPlatform] = useState(readPlatform)
  useEffect(() => {
    if (desktopShell) document.documentElement.dataset.shell = desktopShell
    const update = () => setPlatform(readPlatform())
    update()
    window.addEventListener('banbu-shell', update)
    return () => window.removeEventListener('banbu-shell', update)
  }, [])
  return platform
}

// Windows 右上角的最小化、最大化、关闭。关闭只是隐藏到托盘（main.rs 处理 CloseRequested）。
export function WindowControls() {
  const w = currentWindow()
  if (!w) return null
  return (
    <div className="window-controls">
      <button type="button" aria-label="最小化" onClick={() => void w.minimize()}>
        <MinusOutlined />
      </button>
      <button type="button" aria-label="最大化" onClick={() => void w.toggleMaximize()}>
        <BorderOutlined />
      </button>
      <button type="button" className="window-close" aria-label="关闭" onClick={() => void w.close()}>
        <CloseOutlined />
      </button>
    </div>
  )
}

/** 现在还在运行的 AI 应用名（Claude Code、Claude Desktop、Codex、ChatGPT、Antigravity）。没有外壳或查询失败时返回 undefined。 */
export async function runningApps(): Promise<string[] | undefined> {
  const invoke = window.__TAURI__?.core?.invoke
  if (!invoke) return undefined
  try {
    return (await invoke('shell_running_apps')) as string[]
  } catch {
    return undefined
  }
}
