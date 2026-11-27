import { useCallback, useEffect } from 'react'
import { App } from 'antd'
import { api, type EgressReport } from '../../api'
import { runningApps } from './windowChrome'

// 出口检测与在线检测：登录、启动账号前先看一眼，异常时拦一下，用户确认两次后可以强制上号。
//   - 出口检测：本机此刻的出口 IP、国家（后台进程请求 Cloudflare 和账号所属服务的 /cdn-cgi/trace）；
//   - 在线检测：这个账号对应的 AI 应用（Claude、Codex / ChatGPT、Antigravity）是否正在本机运行，
//     运行中换登录会让正在用的会话掉线或串号。

const kindApps: Record<string, (name: string) => boolean> = {
  claude: (n) => n.startsWith('Claude'),
  codex: (n) => n === 'Codex' || n === 'ChatGPT',
  gemini: (n) => n === 'Antigravity',
}

/** 问题清单：出口异常的原因，加上这个账号对应的应用是否在运行。检查本身失败也算问题。 */
export async function collectProblems(kind: string, checkRunning: boolean): Promise<string[]> {
  const problems: string[] = []
  const [egress, apps] = await Promise.all([
    api.egress.check(kind).then(
      (r: EgressReport) => r.problems,
      (e: Error) => [`出口检测失败：${e.message}`],
    ),
    checkRunning ? runningApps() : Promise.resolve(undefined),
  ])
  problems.push(...egress)
  const match = kindApps[kind]
  if (match && apps) {
    const running = apps.filter(match)
    if (running.length > 0) problems.push(`${running.join('、')} 正在运行，现在换登录可能让正在用的会话掉线或串号，建议先关掉`)
  }
  return problems
}

/**
 * 返回 guard(kind, checkRunning)：检测通过返回 true；有异常时弹窗列出，用户点「强制上号」后再确认一次才返回 true，
 * 取消返回 false。
 */
export function useEgressGuard() {
  const { message, modal } = App.useApp()
  return useCallback(
    async (kind: string, checkRunning: boolean): Promise<boolean> => {
      const hide = message.loading('正在检测出口与在线状态…', 0)
      let problems: string[]
      try {
        problems = await collectProblems(kind, checkRunning)
      } finally {
        hide()
      }
      if (problems.length === 0) return true
      return new Promise<boolean>((resolve) => {
        modal.confirm({
          title: '检测到异常',
          content: (
            <ul style={{ margin: 0, paddingLeft: 18 }}>
              {problems.map((p) => (
                <li key={p}>{p}</li>
              ))}
            </ul>
          ),
          okText: '强制上号',
          okButtonProps: { danger: true },
          cancelText: '取消',
          onCancel: () => resolve(false),
          onOk: () => {
            modal.confirm({
              title: '确认强制上号？',
              content: '出口异常时登录，账号可能被风控或封禁；应用在运行时换登录可能让会话掉线。确认要无视上面的异常继续吗？',
              okText: '我已知晓，继续',
              okButtonProps: { danger: true },
              cancelText: '返回',
              onOk: () => resolve(true),
              onCancel: () => resolve(false),
            })
          },
        })
      })
    },
    [message, modal],
  )
}

// 每次打开应用只检测一次出口（只提示、不拦截）；精简页挂载时触发，重复挂载不会再查。
let startupChecked = false

export function StartupEgressCheck() {
  const { notification } = App.useApp()
  useEffect(() => {
    if (startupChecked) return
    startupChecked = true
    void api.egress.check('').then(
      (r) => {
        if (r.abnormal) {
          notification.warning({ message: '出口检测发现异常', description: r.problems.join('；'), duration: 10, placement: 'bottomRight' })
        }
      },
      () => undefined,
    )
  }, [notification])
  return null
}
