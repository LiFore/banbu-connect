import { useCallback, useEffect, useState } from 'react'
import { Alert, App, Button, Card, Input, Skeleton, Switch, Tooltip } from 'antd'
import { DeleteOutlined, SaveOutlined, UndoOutlined, WifiOutlined } from '@ant-design/icons'
import { api, type NetworkConfig } from '../../api'

const lines = (text: string) =>
  text
    .split(/[\r\n,，]+/)
    .map((v) => v.trim())
    .filter(Boolean)

interface Draft {
  ssids: string
  egress: string
  countries: string
  probes: string
  baselines: Record<string, string>
  autoStart: boolean
  gate: boolean
}

const toDraft = (c: NetworkConfig): Draft => ({
  ssids: (c.trusted_ssids ?? []).join('\n'),
  egress: (c.expected_egress ?? []).join('\n'),
  countries: (c.allowed_countries ?? []).join(', '),
  probes: (c.probe_urls ?? []).join('\n'),
  baselines: { ...(c.baselines ?? {}) },
  autoStart: !!c.auto_start_proxy,
  gate: !!c.gate_direct,
})

// 受信网络：在这些 Wi-Fi 上、出口符合要求时直连，代理交给路由盒；其他网络必须经本机内核。
export default function TrustedNetwork() {
  const { message } = App.useApp()
  const [config, setConfig] = useState<NetworkConfig>()
  const [draft, setDraft] = useState<Draft>()
  const [dirty, setDirty] = useState(false)
  const [saving, setSaving] = useState(false)

  const reset = useCallback((c: NetworkConfig) => {
    setConfig(c)
    setDraft(toDraft(c))
    setDirty(false)
  }, [])

  useEffect(() => {
    api.network.config().then(reset, (e: Error) => message.error(e.message))
  }, [message, reset])

  const patch = (change: Partial<Draft>) => {
    setDraft((d) => d && { ...d, ...change })
    setDirty(true)
  }

  const save = async () => {
    if (!draft) return
    setSaving(true)
    try {
      reset(
        await api.network.update({
          trusted_ssids: draft.ssids
            .split(/\r?\n/)
            .map((v) => v.trim())
            .filter(Boolean),
          expected_egress: lines(draft.egress),
          allowed_countries: lines(draft.countries.replace(/\s+/g, ',')),
          probe_urls: lines(draft.probes),
          baselines: draft.baselines,
          auto_start_proxy: draft.autoStart,
          gate_direct: draft.gate,
        }),
      )
      message.success('已保存')
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  const baselines = Object.entries(draft?.baselines ?? {})

  return (
    <Card
      title={
        <span className="card-title">
          <WifiOutlined />
          受信网络
        </span>
      }
      extra={
        <div className="page-toolbar">
          <Button icon={<UndoOutlined />} disabled={!dirty} onClick={() => config && reset(config)}>
            放弃修改
          </Button>
          <Button type="primary" icon={<SaveOutlined />} disabled={!dirty} loading={saving} onClick={save}>
            保存
          </Button>
        </div>
      }
    >
      <div className="hint">
        在受信的 Wi-Fi 上，出口探测符合要求时直连放行，代理交给路由盒；出口变化或探测失败时阻断。其他网络上必须经本机内核。TUN
        接管流量时总是经内核。留空表示不启用。
      </div>
      {!draft ? (
        <Skeleton active paragraph={{ rows: 4 }} />
      ) : (
        <div className="network-fields">
          <label>
            <span className="hint">受信的 Wi-Fi 名称（每行一个，区分大小写）</span>
            <Input.TextArea
              autoSize={{ minRows: 2, maxRows: 8 }}
              value={draft.ssids}
              spellCheck={false}
              onChange={(e) => patch({ ssids: e.target.value })}
            />
          </label>
          <label>
            <span className="hint">允许的出口 IP 或网段（可选；留空时记住每个 Wi-Fi 第一次的出口）</span>
            <Input.TextArea
              className="mono-input"
              autoSize={{ minRows: 2, maxRows: 8 }}
              value={draft.egress}
              spellCheck={false}
              onChange={(e) => patch({ egress: e.target.value })}
            />
          </label>
          <label>
            <span className="hint">允许的出口地区（可选，两位代码，如 US, JP）</span>
            <Input
              className="mono-input"
              value={draft.countries}
              spellCheck={false}
              onChange={(e) => patch({ countries: e.target.value })}
            />
          </label>
          <label>
            <span className="hint">出口探测地址（https，返回 Cloudflare trace 或纯 IP）</span>
            <Input.TextArea
              className="mono-input"
              autoSize={{ minRows: 1, maxRows: 4 }}
              value={draft.probes}
              spellCheck={false}
              onChange={(e) => patch({ probes: e.target.value })}
            />
          </label>
          <div className="setting-row">
            <span>
              离开受信网络时自动启动代理
              <div className="hint">
                从受信的 Wi-Fi 切换到其他 Wi-Fi 或有线网络时，如果本机代理没有运行，自动启动它并发系统通知。
              </div>
            </span>
            <Switch
              aria-label="离开受信网络时自动启动代理"
              checked={draft.autoStart}
              onChange={(v) => patch({ autoStart: v })}
            />
          </div>
          <div className="setting-row">
            <span>
              代理常开，受信网络上经出口闸门直连
              <div className="hint">
                本机代理和 TUN 保持运行。在受信的 Wi-Fi 上，受保护服务的每个新连接先由出口闸门确认它自己的出口（如
                claude.ai、chatgpt.com 各自的 Cloudflare trace，10 秒内复用），符合才直连交给路由盒，否则经代理。换网络时不再有没有规则的空档。
              </div>
            </span>
            <Switch
              aria-label="代理常开，受信网络上经出口闸门直连"
              checked={draft.gate}
              onChange={(v) => patch({ gate: v })}
            />
          </div>
          {draft.gate && !draft.egress.trim() && !draft.countries.trim() && (
            <Alert
              type="warning"
              showIcon
              title="建议设置允许的出口或出口地区"
              description="不设置时，每个服务第一次确认到的出口会被记为基准：如果那时路由盒没在代理，记下的就是真实出口。"
            />
          )}
          {baselines.length > 0 && (
            <div className="network-baselines">
              <span className="hint">记住的出口（删除后下次探测重新记录）</span>
              {baselines.map(([ssid, ip]) => (
                <div className="setting-row" key={ssid}>
                  <span className="selectable">{ssid.includes('#') ? ssid.replace(/#([^#]*)$/, ' · $1') : ssid}</span>
                  <span className="setting-control">
                    <code>{ip}</code>
                    <Tooltip title="删除（保存后生效）">
                      <Button
                        danger
                        type="text"
                        size="small"
                        aria-label={`删除 ${ssid} 的出口`}
                        icon={<DeleteOutlined />}
                        onClick={() => {
                          const next = { ...draft.baselines }
                          delete next[ssid]
                          patch({ baselines: next })
                        }}
                      />
                    </Tooltip>
                  </span>
                </div>
              ))}
            </div>
          )}
        </div>
      )}
    </Card>
  )
}
