import { useState } from 'react'
import { App, Button, Card } from 'antd'
import { DeleteOutlined } from '@ant-design/icons'
import { api } from '../../api'
import { quitApp } from './windowChrome'

// 高级 · 卸载：删除后台服务。平时「退出」只暂停代理、保留服务（下次打开不用重新安装）；
// 要彻底移除时在这里卸载，服务在自己的独立进程里停止并删除，保护规则撤销，数据保留。
export default function Uninstall() {
  const { message, modal } = App.useApp()
  const [busy, setBusy] = useState(false)

  const uninstall = () =>
    modal.confirm({
      title: '卸载后台服务？',
      content: (
        <>
          <p>会停止代理、删除后台服务并撤销保护规则，应用随后退出。设置、节点等数据保留，重新安装后可以继续使用。</p>
          <p>要删除应用本身，卸载后把 Banbu Connect 从「应用程序」（Windows：设置 · 应用）里移除。</p>
        </>
      ),
      okText: '卸载',
      okButtonProps: { danger: true },
      cancelText: '取消',
      onOk: async () => {
        setBusy(true)
        try {
          await api.desktop.uninstall()
          message.success('后台服务正在卸载，应用即将退出')
          // 服务在删除，不用再暂停代理。
          setTimeout(() => {
            if (!quitApp(false)) setBusy(false)
          }, 1500)
        } catch (e) {
          setBusy(false)
          message.error((e as Error).message)
        }
      },
    })

  return (
    <Card title="卸载">
      <p className="hint">
        菜单和精简界面里的「退出」只停止代理，后台服务保留，下次打开应用时自动恢复，不用重新安装。要彻底移除后台服务时在这里卸载。
      </p>
      <Button danger icon={<DeleteOutlined />} loading={busy} onClick={uninstall}>
        卸载后台服务
      </Button>
    </Card>
  )
}
