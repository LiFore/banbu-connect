import { useEffect, useState } from 'react'
import { App, Button, Card, Input, InputNumber, Skeleton } from 'antd'
import { CloudDownloadOutlined, FileTextOutlined, GlobalOutlined, SaveOutlined, UndoOutlined } from '@ant-design/icons'
import { api } from '../../api'
import { PageHeader } from '../common'

export default function Settings() {
  const { message } = App.useApp()
  const [rules, setRules] = useState('')
  const [port, setPort] = useState<number>()
  const [mirror, setMirror] = useState('')
  const [saved, setSaved] = useState<{ rules: string; port?: number; mirror: string }>({
    rules: '',
    mirror: '',
  })
  const [loaded, setLoaded] = useState(false)
  const [dirty, setDirty] = useState(false)
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    api.desktop.settings().then(
      (s) => {
        const text = (s.custom_rules ?? []).join('\n')
        setRules(text)
        setPort(s.mixed_port)
        setMirror(s.mirror ?? '')
        setSaved({ rules: text, port: s.mixed_port, mirror: s.mirror ?? '' })
        setLoaded(true)
      },
      (e: Error) => message.error(e.message),
    )
  }, [message])

  const save = async () => {
    setSaving(true)
    try {
      const s = await api.desktop.updateSettings({
        custom_rules: rules.split(/\r?\n/),
        mixed_port: port,
        mirror,
      })
      setMirror(s.mirror ?? '')
      setSaved({ rules, port, mirror: s.mirror ?? '' })
      setDirty(false)
      message.success('已保存；校验结果见概览页')
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  if (!loaded) {
    return (
      <Card>
        <Skeleton active paragraph={{ rows: 6 }} />
      </Card>
    )
  }

  return (
    <div className="desktop-stack">
      <PageHeader
        title="规则与设置"
        actions={
          <>
            <Button
              icon={<UndoOutlined />}
              disabled={!dirty}
              onClick={() => {
                setRules(saved.rules)
                setPort(saved.port)
                setMirror(saved.mirror)
                setDirty(false)
              }}
            >
              放弃修改
            </Button>
            <Button type="primary" icon={<SaveOutlined />} disabled={!dirty} loading={saving} onClick={save}>
              保存
            </Button>
          </>
        }
      />
      <Card
        title={
          <span className="card-title">
            <FileTextOutlined />
            自定义规则
          </span>
        }
      >
        <p className="hint rules-note">
          每行一条，优先级最高，语法与 Mihomo 相同，如 <code>DOMAIN-SUFFIX,example.com,代理模式</code>。# 开头为注释。
          受保护服务的规则会自动生成，不用写在这里。写错时内核继续使用上一份配置，错误显示在概览页。
        </p>
        <Input.TextArea
          aria-label="自定义规则"
          className="mono-input"
          value={rules}
          spellCheck={false}
          autoSize={{ minRows: 8, maxRows: 24 }}
          onChange={(e) => {
            setRules(e.target.value)
            setDirty(true)
          }}
        />
      </Card>
      <Card>
        <div className="setting-row">
          <span className="card-title">
            <GlobalOutlined />
            本地代理端口
          </span>
          <InputNumber
            aria-label="本地代理端口"
            min={1024}
            max={65535}
            value={port}
            onChange={(v) => {
              setPort(v ?? undefined)
              setDirty(true)
            }}
          />
        </div>
      </Card>
      <Card>
        <div className="setting-row">
          <span className="card-title">
            <CloudDownloadOutlined />
            下载镜像
          </span>
          <Input
            aria-label="下载镜像"
            placeholder="https://镜像主机/路径前缀，留空不用"
            style={{ maxWidth: 360 }}
            value={mirror}
            onChange={(e) => {
              setMirror(e.target.value)
              setDirty(true)
            }}
          />
        </div>
        <p className="hint">
          规则集和 geo 数据默认从 jsDelivr 下载。国内直连不稳定时填一个与 jsDelivr 路径兼容的镜像（如自建反代），
          <code>https://cdn.jsdelivr.net/gh/…</code> 会改成 <code>镜像/gh/…</code>；其他地址不变。
        </p>
      </Card>
    </div>
  )
}
