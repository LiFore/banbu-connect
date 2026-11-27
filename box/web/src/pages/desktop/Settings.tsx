import { useEffect, useState } from 'react'
import { App, Button, Card, Input, InputNumber, Skeleton, Switch } from 'antd'
import { CloudDownloadOutlined, FileTextOutlined, GlobalOutlined, SaveOutlined, UndoOutlined } from '@ant-design/icons'
import { api, isBox } from '../../api'
import { PageHeader } from '../common'

export default function Settings() {
  const { message } = App.useApp()
  const [rules, setRules] = useState('')
  const [port, setPort] = useState<number>()
  const [mirror, setMirror] = useState('')
  const [allowLAN, setAllowLAN] = useState(false)
  const [saved, setSaved] = useState<{ rules: string; port?: number; mirror: string; allowLAN: boolean }>({
    rules: '',
    mirror: '',
    allowLAN: false,
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
        setAllowLAN(!!s.allow_lan)
        setSaved({ rules: text, port: s.mixed_port, mirror: s.mirror ?? '', allowLAN: !!s.allow_lan })
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
        ...(isBox() ? { allow_lan: allowLAN } : {}),
      })
      setMirror(s.mirror ?? '')
      setAllowLAN(!!s.allow_lan)
      setSaved({ rules, port, mirror: s.mirror ?? '', allowLAN: !!s.allow_lan })
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
                setAllowLAN(saved.allowLAN)
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
            {isBox() ? '代理端口' : '本地代理端口'}
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
        {isBox() && (
          <>
            <div className="setting-row">
              <span>允许局域网连接代理端口</span>
              <Switch
                aria-label="允许局域网连接代理端口"
                checked={allowLAN}
                onChange={(v) => {
                  setAllowLAN(v)
                  setDirty(true)
                }}
              />
            </div>
            <p className="hint">
              局域网设备默认把网关指向路由盒即可，不用填代理。打开后，设备也可以手动填 HTTP / SOCKS5 代理
              <code>路由盒地址:{port ?? 7890}</code>，规则和受保护服务的限制照常生效。只接受局域网网段（含
              route_exclude 里的网段）的连接，不对外网开放；代理端口没有密码，局域网里任何设备都能用。
            </p>
          </>
        )}
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
