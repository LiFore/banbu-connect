import { useEffect, useState } from 'react'
import { Button, Checkbox, Modal, Tag } from 'antd'
import { CheckOutlined, ExclamationOutlined, GlobalOutlined, LoadingOutlined, WarningFilled } from '@ant-design/icons'
import { runningApps } from './windowChrome'

// 精简页「退出」的确认框（ADR-090）。退出会暂停代理，之后的流量直接从本机网络出去，真实地址完全暴露，
// 所以要醒目提醒（动画）并二次确认：
// 1. 现查还在运行的 Claude、Codex、ChatGPT、Antigravity（外壳命令 shell_running_apps），打开期间每 2 秒刷新；
// 2. 提醒检查浏览器里没关的网页；
// 3. 勾选「已关闭」后才能点「退出」。
// 只在要退出时挂载（调用方 {open && <QuitConfirm />}），每次打开都是新的状态。

export default function QuitConfirm({ onCancel, onQuit }: { onCancel: () => void; onQuit: () => void }) {
  const [apps, setApps] = useState<string[] | null | undefined>(null) // undefined：查不到（没有外壳）；null：检查中
  const [ack, setAck] = useState(false)

  useEffect(() => {
    let alive = true
    const check = () => void runningApps().then((list) => alive && setApps(list))
    check()
    const timer = setInterval(check, 2000)
    return () => {
      alive = false
      clearInterval(timer)
    }
  }, [])

  const running = apps && apps.length > 0
  return (
    <Modal
      open
      onCancel={onCancel}
      centered
      width={380}
      className="quit-modal"
      title="退出 Banbu Connect"
      footer={
        <div className="quit-footer">
          <Button onClick={onCancel} autoFocus>
            取消
          </Button>
          <Button type="primary" danger disabled={!ack} onClick={onQuit}>
            {running ? '仍然退出' : '退出'}
          </Button>
        </div>
      }
    >
      <div className="quit-alarm" role="alert">
        <span className="quit-alarm-icon" aria-hidden>
          <WarningFilled />
        </span>
        <div>
          <div className="quit-alarm-title">退出后将完全暴露真实地址</div>
          <div className="quit-alarm-desc">代理会暂停，所有流量直接从本机网络出去，对方看到的是你的真实 IP。下次打开应用时恢复。</div>
        </div>
      </div>

      <ul className="quit-list">
        <li className="quit-item" data-state={apps === null ? 'loading' : running ? 'bad' : apps === undefined ? 'warn' : 'ok'}>
          <span className="quit-item-icon" aria-hidden>
            {apps === null ? <LoadingOutlined /> : running || apps === undefined ? <ExclamationOutlined /> : <CheckOutlined />}
          </span>
          <div className="quit-item-body">
            <div className="quit-item-title">在运行的 AI 应用</div>
            {apps === null ? (
              <div className="quit-item-note">正在检查…</div>
            ) : apps === undefined ? (
              <div className="quit-item-note">无法检查，请自己确认 Claude、Codex、ChatGPT、Antigravity 都已退出</div>
            ) : running ? (
              <>
                <div className="quit-apps">
                  {apps.map((a) => (
                    <Tag key={a} color="error" className="quit-app">
                      {a}
                    </Tag>
                  ))}
                </div>
                <div className="quit-item-note">请先退出这些应用，否则它们会用真实地址继续连接。</div>
              </>
            ) : (
              <div className="quit-item-note">没有发现在运行的 Claude、Codex、ChatGPT、Antigravity</div>
            )}
          </div>
        </li>
        <li className="quit-item" data-state="warn">
          <span className="quit-item-icon" aria-hidden>
            <GlobalOutlined />
          </span>
          <div className="quit-item-body">
            <div className="quit-item-title">浏览器里的网页</div>
            <div className="quit-item-note">请检查是否还开着 claude.ai、chatgpt.com、gemini.google.com 等网页，先关掉再退出。</div>
          </div>
        </li>
      </ul>

      <Checkbox className="quit-ack" checked={ack} onChange={(e) => setAck(e.target.checked)}>
        我已关闭上面的应用和网页，确认退出
      </Checkbox>
    </Modal>
  )
}
