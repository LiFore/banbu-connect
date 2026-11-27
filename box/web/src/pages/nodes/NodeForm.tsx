import { useMemo, useState } from 'react'
import {
  Alert,
  Button,
  Collapse,
  Descriptions,
  Flex,
  Form,
  Input,
  InputNumber,
  Modal,
  Segmented,
  Select,
  Space,
  Switch,
  Typography,
} from 'antd'
import {
  api,
  SECRET_MASK,
  type Mapping,
  type NodeDetail,
  type NodeInput,
  type GroupView,
  type ProtocolSchema,
  type ProxyNode,
  type SchemaField,
  type TestResult,
} from '../../api'
import { defaults, fromForm, toForm } from './mapping'
import TestResultView from './TestResultView'

interface Props {
  editing?: NodeDetail // 为空表示新建
  schemas: ProtocolSchema[]
  regions: string[]
  allNodes: ProxyNode[]
  groups: GroupView[]
  onClose: () => void
  onSaved: () => void
}

interface Values {
  kind: 'manual' | 'isp'
  display_name: string
  region: string
  protocol: string
  dialer_ref: string
  tags: string[]
  enabled: boolean
  fields: Record<string, unknown>
}

function FieldInput({ f, editing }: { f: SchemaField; editing: boolean }) {
  switch (f.type) {
    case 'secret':
      return <Input.Password placeholder={editing ? `${SECRET_MASK} 表示不修改` : undefined} autoComplete="new-password" />
    case 'int':
      return <InputNumber style={{ width: '100%' }} min={0} />
    case 'bool':
      return <Switch />
    case 'select':
      return <Select allowClear options={f.options?.map((o) => ({ value: o, label: o || '（无）' }))} />
    case 'list':
      return <Select mode="tags" tokenSeparators={[',']} />
    default:
      return <Input />
  }
}

function fieldItems(fs: SchemaField[], editing: boolean) {
  return fs.map((f) => (
    <Form.Item
      key={f.key}
      name={['fields', f.key]}
      label={f.label}
      extra={f.help}
      valuePropName={f.type === 'bool' ? 'checked' : 'value'}
      rules={f.required ? [{ required: true, message: `请填写${f.label}` }] : []}
    >
      <FieldInput f={f} editing={editing} />
    </Form.Item>
  ))
}

export default function NodeForm({ editing, schemas, regions, allNodes, groups, onClose, onSaved }: Props) {
  const [form] = Form.useForm<Values>()
  const [error, setError] = useState<string>()
  const [saving, setSaving] = useState(false)
  const [testing, setTesting] = useState<'delay' | 'exit'>()
  const [result, setResult] = useState<{ ok: boolean; kind: 'delay' | 'exit'; res?: TestResult; msg?: string }>()
  const isSub = editing?.kind === 'subscription'

  const initialProtocol = editing?.protocol ?? 'vless'
  const [protocol, setProtocol] = useState(initialProtocol)
  const schema = schemas.find((s) => s.type === protocol)

  const initialValues = useMemo<Values>(() => {
    const s = schemas.find((x) => x.type === initialProtocol)
    return {
      kind: editing?.kind === 'isp' ? 'isp' : 'manual',
      display_name: editing?.display_name ?? '',
      region: editing?.region ?? '',
      protocol: initialProtocol,
      dialer_ref: editing?.dialer_ref ?? '',
      tags: editing?.tags ?? [],
      enabled: editing?.enabled ?? true,
      fields: s ? (editing ? toForm(editing.mapping, s) : defaults(s)) : {},
    }
  }, [editing, schemas, initialProtocol])

  const dialerOptions = [
    { label: '直连', options: [{ value: '', label: '（不使用前置代理）' }] },
    { label: '代理组', options: groups.map((g) => ({ value: g.id, label: g.name })) },
    {
      label: '节点',
      options: allNodes
        .filter((n) => n.id !== editing?.id)
        .map((n) => ({ value: n.id, label: `${n.display_name}${n.subscription_name ? `（${n.subscription_name}）` : ''}` })),
    },
  ]

  const currentMapping = (v: Values): Mapping => {
    const base = editing?.mapping ?? {}
    return schema ? fromForm(base, protocol, schema, v.fields ?? {}) : base
  }

  const runTest = async (kind: 'delay' | 'exit') => {
    let v: Values
    try {
      v = isSub ? form.getFieldsValue(true) : await form.validateFields()
    } catch {
      return
    }
    setTesting(kind)
    setResult(undefined)
    try {
      const res = await api.nodes.test({
        kind,
        id: editing?.id,
        mapping: isSub ? undefined : currentMapping(v),
        dialer_ref: v.dialer_ref ?? '',
      })
      setResult({ ok: true, kind, res })
    } catch (e) {
      setResult({ ok: false, kind, msg: (e as Error).message })
    } finally {
      setTesting(undefined)
    }
  }

  const submit = async (v: Values) => {
    setSaving(true)
    setError(undefined)
    try {
      if (isSub) {
        await api.nodes.patch(editing!.id, { enabled: v.enabled, tags: v.tags, dialer_ref: v.dialer_ref ?? '' })
      } else {
        const input: NodeInput = {
          kind: v.kind,
          display_name: v.display_name,
          region: v.region ?? '',
          mapping: currentMapping(v),
          dialer_ref: v.dialer_ref ?? '',
          tags: v.tags ?? [],
          enabled: v.enabled,
        }
        if (editing) await api.nodes.update(editing.id, input)
        else await api.nodes.create(input)
      }
      onSaved()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  const basic = schema?.fields.filter((f) => !f.advanced) ?? []
  const advanced = schema?.fields.filter((f) => f.advanced) ?? []

  return (
    <Modal
      title={editing ? `编辑节点：${editing.display_name}` : '添加节点'}
      open
      width={680}
      onCancel={onClose}
      footer={
        <Space>
          <Button loading={testing === 'delay'} onClick={() => runTest('delay')}>
            测延迟
          </Button>
          <Button loading={testing === 'exit'} onClick={() => runTest('exit')}>
            出口检测
          </Button>
          <Button onClick={onClose}>取消</Button>
          <Button type="primary" loading={saving} onClick={() => form.submit()}>
            保存
          </Button>
        </Space>
      }
    >
      {error && <Alert type="error" showIcon title={error} style={{ marginBottom: 16 }} />}
      {result && (
        <div style={{ marginBottom: 16 }}>
          <TestResultView {...result} />
        </div>
      )}
      <Form form={form} layout="vertical" initialValues={initialValues} onFinish={submit}>
        {isSub ? (
          <Descriptions column={2} size="small" style={{ marginBottom: 16 }}>
            <Descriptions.Item label="来源">订阅 {editing!.subscription_name}</Descriptions.Item>
            <Descriptions.Item label="协议">{editing!.protocol}</Descriptions.Item>
            <Descriptions.Item label="地址">
              {editing!.server}:{editing!.port}
            </Descriptions.Item>
            <Descriptions.Item label="地区">{editing!.region}</Descriptions.Item>
            <Descriptions.Item label="说明" span={2}>
              <Typography.Text type="secondary">配置随订阅更新覆盖，这里只能修改启用、前置代理与标签。</Typography.Text>
            </Descriptions.Item>
          </Descriptions>
        ) : (
          <>
            <Flex gap={12}>
              <Form.Item name="kind" label="类型" style={{ flex: 1 }}>
                <Segmented
                  block
                  options={[
                    { value: 'manual', label: '自建隧道' },
                    { value: 'isp', label: 'ISP 代理' },
                  ]}
                />
              </Form.Item>
              <Form.Item name="protocol" label="协议" style={{ flex: 1 }}>
                <Select
                  disabled={!!editing}
                  options={schemas.map((s) => ({ value: s.type, label: s.label }))}
                  onChange={(p: string) => {
                    setProtocol(p)
                    const s = schemas.find((x) => x.type === p)
                    if (s) form.setFieldValue('fields', defaults(s))
                  }}
                />
              </Form.Item>
            </Flex>
            <Flex gap={12}>
              <Form.Item
                name="display_name"
                label="名称"
                rules={[{ required: true, message: '请输入名称' }]}
                style={{ flex: 2 }}
              >
                <Input maxLength={64} placeholder="如 自建 东京" />
              </Form.Item>
              <Form.Item name="region" label="地区" style={{ flex: 1 }}>
                <Select
                  showSearch
                  options={[{ value: '', label: '按名称自动识别' }, ...regions.map((r) => ({ value: r, label: r }))]}
                />
              </Form.Item>
            </Flex>
            {schema ? (
              <>
                {fieldItems(basic, !!editing)}
                {advanced.length > 0 && (
                  <Collapse
                    style={{ marginBottom: 16 }}
                    items={[{ key: 'adv', label: '高级', forceRender: true, children: fieldItems(advanced, !!editing) }]}
                  />
                )}
              </>
            ) : (
              <Alert
                type="info"
                showIcon
                style={{ marginBottom: 16 }}
                title={`${protocol} 协议没有表单`}
                description="可修改名称、地区、前置代理与标签；如需改配置，请删除后重新导入。"
              />
            )}
          </>
        )}
        <Form.Item
          name="dialer_ref"
          label="前置代理（dialer-proxy）"
          extra="流量先经前置代理再连到本节点，如：ISP 代理经机场节点或「链式拨号代理组」出去。前置为代理组时，测试使用组内首个节点。"
        >
          <Select showSearch optionFilterProp="label" options={dialerOptions} />
        </Form.Item>
        <Flex gap={12}>
          <Form.Item name="tags" label="标签" style={{ flex: 3 }}>
            <Select mode="tags" tokenSeparators={[',']} />
          </Form.Item>
          <Form.Item name="enabled" label="启用" valuePropName="checked" style={{ flex: 1 }}>
            <Switch />
          </Form.Item>
        </Flex>
      </Form>
    </Modal>
  )
}
