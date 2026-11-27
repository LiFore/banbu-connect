import { useState } from 'react'
import { Alert, Button, Collapse, Flex, Form, Input, InputNumber, Modal, Segmented, Select, Space, Switch } from 'antd'
import { api, type SubDefaults, type Subscription, type SubscriptionInput } from '../../api'

interface Props {
  editing?: Subscription // 为空表示新建
  defaults: SubDefaults
  onClose: () => void
  onSaved: () => void
}

// 由父组件在打开时挂载、关闭时卸载，因此初始值只需在挂载时给出。
export default function SubscriptionForm({ editing, defaults, onClose, onSaved }: Props) {
  const [form] = Form.useForm<SubscriptionInput>()
  const [error, setError] = useState<string>()
  const [saving, setSaving] = useState(false)
  const initialValues: SubscriptionInput = editing
    ? { ...editing, url: '' }
    : {
        name: '',
        display_name: '',
        url: '',
        user_agent: defaults.user_agent,
        fetch_via: 'direct',
        enabled: true,
        options: defaults.options,
      }

  const revealURL = async () => {
    if (!editing) return
    try {
      const s = await api.subscriptions.get(editing.id, true)
      form.setFieldValue('url', s.url)
    } catch (e) {
      setError((e as Error).message)
    }
  }

  const submit = async (v: SubscriptionInput) => {
    setSaving(true)
    setError(undefined)
    try {
      if (editing) await api.subscriptions.update(editing.id, v)
      else await api.subscriptions.create(v)
      onSaved()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <Modal
      title={editing ? `编辑订阅：${editing.name}` : '添加订阅'}
      open
      onCancel={onClose}
      onOk={() => form.submit()}
      confirmLoading={saving}
      width={640}
    >
      {error && <Alert type="error" showIcon title={error} style={{ marginBottom: 16 }} />}
      <Form form={form} layout="vertical" onFinish={submit} initialValues={initialValues}>
        <Flex gap={12}>
          <Form.Item
            name="name"
            label="名称（用于显示名中的 {sub}）"
            rules={[{ required: true, message: '请输入名称' }]}
            style={{ flex: 1 }}
          >
            <Input placeholder="如 jsyun" maxLength={32} />
          </Form.Item>
          <Form.Item name="display_name" label="显示名称" style={{ flex: 1 }}>
            <Input placeholder="如 极速云 月付" maxLength={64} />
          </Form.Item>
        </Flex>
        <Form.Item
          name="url"
          label="订阅地址"
          rules={editing ? [] : [{ required: true, message: '请输入订阅地址' }]}
          extra={editing ? '留空表示不修改。地址含访问令牌，默认不显示。' : undefined}
        >
          <Input.Password
            placeholder={editing ? '（已保存，留空不修改）' : 'https://...'}
            visibilityToggle
            addonAfter={editing ? <Button type="link" size="small" onClick={revealURL}>显示当前地址</Button> : undefined}
          />
        </Form.Item>
        <Form.Item name="fetch_via" label="拉取方式">
          <Segmented
            block
            options={[
              { value: 'direct', label: '直连' },
              { value: 'proxy', label: '经代理（Mihomo）' },
            ]}
          />
        </Form.Item>
        <Form.Item name="enabled" label="启用" valuePropName="checked">
          <Switch />
        </Form.Item>
        <Collapse
          items={[
            {
              key: 'advanced',
              label: '清洗与高级设置',
              forceRender: true,
              children: (
                <>
                  <Form.Item
                    name={['options', 'filter_patterns']}
                    label="信息节点过滤（名称匹配任一正则即丢弃）"
                  >
                    <Select mode="tags" tokenSeparators={[',']} placeholder="输入后回车" />
                  </Form.Item>
                  <Form.Item
                    name={['options', 'name_template']}
                    label="显示名模板"
                    rules={[{ required: true }, { pattern: /\{seq/, message: '必须包含 {seq}' }]}
                    extra="变量：{region} 地区、{seq} / {seq:02} 序号、{sub} 订阅名、{name} 原名、{protocol} 协议"
                  >
                    <Input />
                  </Form.Item>
                  <Form.Item name={['options', 'keep_unmatched']} label="地区无法识别的节点" valuePropName="checked">
                    <Switch checkedChildren="归入“其他”" unCheckedChildren="丢弃" />
                  </Form.Item>
                  <Space>
                    <Form.Item name={['options', 'timeout_sec']} label="超时（秒）">
                      <InputNumber min={1} max={300} />
                    </Form.Item>
                    <Form.Item name={['options', 'retries']} label="重试次数">
                      <InputNumber min={0} max={5} />
                    </Form.Item>
                    <Form.Item name={['options', 'update_hours']} label="自动更新（小时）" tooltip="0 为不自动更新。失败时保留上一次的节点，原因显示在概览页">
                      <InputNumber min={0} max={168} />
                    </Form.Item>
                  </Space>
                  <Form.Item name="user_agent" label="User-Agent">
                    <Input />
                  </Form.Item>
                </>
              ),
            },
          ]}
        />
      </Form>
    </Modal>
  )
}
