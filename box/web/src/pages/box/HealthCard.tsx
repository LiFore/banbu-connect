import { useState } from 'react'
import { App, Button, Card, InputNumber } from 'antd'
import { HeartOutlined, SaveOutlined } from '@ant-design/icons'
import { api, type StrictHealth } from '../../api'

const fields: { key: keyof StrictHealth; label: string; unit: string; min: number; max: number }[] = [
  { key: 'interval', label: '正常时探测间隔', unit: '秒', min: 10, max: 600 },
  { key: 'blocked_interval', label: '阻断时探测间隔', unit: '秒', min: 5, max: 120 },
  { key: 'failures', label: '连续失败几次后阻断', unit: '次', min: 1, max: 10 },
  { key: 'recover', label: '连续成功几次后恢复', unit: '次', min: 1, max: 10 },
]

// 严格线路检查的阈值（M9-05）。只在管理服务下发了允许的出口时影响阻断。
export default function HealthCard({ value, onSaved }: { value?: StrictHealth; onSaved: () => void }) {
  const { message } = App.useApp()
  const [edited, setForm] = useState<StrictHealth>()
  const [saving, setSaving] = useState(false)
  // 没改过时跟随服务端的值（页面每 3 秒刷新）。
  const form = edited ?? value

  const dirty = !!form && !!value && fields.some((f) => form[f.key] !== value[f.key])

  const save = async () => {
    if (!form) return
    setSaving(true)
    try {
      const cfg = await api.box.setHealth(form)
      if (cfg.health) setForm(cfg.health)
      message.success('已保存')
      onSaved()
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <Card
      title={
        <span className="card-title">
          <HeartOutlined />
          严格线路检查
        </span>
      }
      extra={
        <Button type="primary" size="small" icon={<SaveOutlined />} disabled={!dirty} loading={saving} onClick={save}>
          保存
        </Button>
      }
    >
      <div className="health-grid">
        {fields.map((f) => (
          <label key={f.key} className="health-field">
            <span>{f.label}</span>
            <InputNumber
              min={f.min}
              max={f.max}
              value={form?.[f.key]}
              disabled={!form}
              suffix={f.unit}
              onChange={(v) => form && v != null && setForm({ ...form, [f.key]: v })}
            />
          </label>
        ))}
      </div>
      <p className="hint">
        管理服务下发了允许的出口时生效：任一严格线路连续探测失败达到次数就阻断全部受保护服务，恢复时需要连续成功达到次数，避免线路来回抖动时反复放行。
      </p>
    </Card>
  )
}
