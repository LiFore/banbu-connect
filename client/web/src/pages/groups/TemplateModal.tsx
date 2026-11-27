import { useState } from 'react'
import { Alert, Form, Modal, Radio, Select, Switch } from 'antd'
import { api, type GroupTemplate } from '../../api'

interface Props {
  regions: string[]
  onClose: () => void
  onDone: (msg: string) => void
}

// 一键生成常用代理组（M4-06），成员均为动态条件，订阅更新后自动跟随。
export default function TemplateModal({ regions, onClose, onDone }: Props) {
  const [template, setTemplate] = useState<GroupTemplate>('region_auto')
  const [selected, setSelected] = useState<string[]>([])
  const [excludeISP, setExcludeISP] = useState(true)
  const [running, setRunning] = useState(false)
  const [error, setError] = useState<string>()

  const run = async () => {
    setRunning(true)
    setError(undefined)
    try {
      const r = await api.groups.template(template, template === 'subscription_auto' ? [] : selected, excludeISP)
      onDone(`新建 ${r.created.length} 个组${r.skipped.length ? `，跳过已存在的 ${r.skipped.length} 个` : ''}`)
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setRunning(false)
    }
  }

  return (
    <Modal title="从模板生成代理组" open onCancel={onClose} onOk={run} okText="生成" confirmLoading={running}>
      {error && <Alert type="error" showIcon title={error} style={{ marginBottom: 16 }} />}
      <Form layout="vertical">
        <Form.Item label="模板">
          <Radio.Group
            value={template}
            onChange={(e) => setTemplate(e.target.value)}
            options={[
              { value: 'region_auto', label: '按地区自动选择（url-test）' },
              { value: 'region_manual', label: '按地区手工选择（select）' },
              { value: 'subscription_auto', label: '按订阅自动选择（url-test）' },
            ]}
          />
        </Form.Item>
        {template !== 'subscription_auto' && (
          <Form.Item label="地区" extra="留空表示所有有节点的地区，按节点数排序">
            <Select mode="multiple" value={selected} onChange={setSelected} options={regions.map((r) => ({ value: r, label: r }))} />
          </Form.Item>
        )}
        <Form.Item label="排除 ISP 代理节点（isp 标签）">
          <Switch checked={excludeISP} onChange={setExcludeISP} />
        </Form.Item>
      </Form>
    </Modal>
  )
}
