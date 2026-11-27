import { useState } from 'react'
import {
  Alert,
  Button,
  Card,
  Collapse,
  Flex,
  Form,
  Input,
  InputNumber,
  Modal,
  Select,
  Space,
  Switch,
  Tag,
  Typography,
} from 'antd'
import {
  api,
  emptyFilter,
  type GroupType,
  type GroupView,
  type ProxyGroup,
  type ProxyNode,
  type Subscription,
} from '../../api'

interface Props {
  editing?: GroupView
  groups: GroupView[]
  nodes: ProxyNode[]
  subscriptions: Subscription[]
  onClose: () => void
  onSaved: () => void
}

type Values = Omit<ProxyGroup, 'id' | 'sort_order'>

const typeOptions: { value: GroupType; label: string }[] = [
  { value: 'select', label: '手动选择（select）' },
  { value: 'url-test', label: '自动选择延迟最低（url-test）' },
  { value: 'fallback', label: '故障转移（fallback）' },
  { value: 'load-balance', label: '负载均衡（load-balance）' },
]

const kindOptions = [
  { value: 'subscription', label: '订阅节点' },
  { value: 'manual', label: '自建隧道' },
  { value: 'isp', label: 'ISP 代理' },
]

function uniq(xs: string[]) {
  return [...new Set(xs)].sort()
}

export default function GroupForm({ editing, groups, nodes, subscriptions, onClose, onSaved }: Props) {
  const [form] = Form.useForm<Values>()
  const [error, setError] = useState<string>()
  const [saving, setSaving] = useState(false)
  const [preview, setPreview] = useState<GroupView>()
  const [previewing, setPreviewing] = useState(false)
  const type = Form.useWatch('type', form) ?? editing?.type ?? 'select'
  const strict = Form.useWatch('strict', form) ?? false
  const filterOn = Form.useWatch(['members', 'filter', 'enabled'], form) ?? false

  const initialValues: Values = editing
    ? { name: editing.name, type: editing.type, strict: editing.strict, members: editing.members, options: editing.options, icon: editing.icon }
    : {
        name: '',
        type: 'select',
        strict: false,
        members: { explicit: [], filter: emptyFilter() },
        options: { empty_fallback: 'REJECT' },
        icon: '',
      }

  const otherGroups = groups.filter((g) => g.id !== editing?.id)
  const memberOptions = [
    { label: '内置', options: [{ value: 'DIRECT', label: 'DIRECT（直连）' }, { value: 'REJECT', label: 'REJECT（拒绝）' }] },
    { label: '代理组', options: otherGroups.map((g) => ({ value: g.id, label: g.name })) },
    {
      label: '节点',
      options: nodes.map((n) => ({
        value: n.id,
        label: `${n.display_name}${n.subscription_name ? `（${n.subscription_name}）` : ''}${n.enabled ? '' : '（已禁用）'}`,
      })),
    },
  ]
  const fallbackOptions = [
    { value: 'REJECT', label: 'REJECT（拒绝，推荐）' },
    { value: 'DIRECT', label: 'DIRECT（直连）', disabled: strict },
    ...otherGroups.map((g) => ({ value: g.id, label: g.name })),
  ]

  const runPreview = async () => {
    setPreviewing(true)
    setError(undefined)
    try {
      setPreview(await api.groups.resolve({ ...form.getFieldsValue(true), id: editing?.id }))
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setPreviewing(false)
    }
  }

  const submit = async (v: Values) => {
    setSaving(true)
    setError(undefined)
    try {
      if (editing) await api.groups.update(editing.id, v)
      else await api.groups.create(v)
      onSaved()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <Modal
      title={editing ? `编辑代理组：${editing.name}` : '新建代理组'}
      open
      width={760}
      onCancel={onClose}
      footer={
        <Space>
          <Button loading={previewing} onClick={runPreview}>
            预览成员
          </Button>
          <Button onClick={onClose}>取消</Button>
          <Button type="primary" loading={saving} onClick={() => form.submit()}>
            保存
          </Button>
        </Space>
      }
    >
      {error && <Alert type="error" showIcon title={error} style={{ marginBottom: 16 }} />}
      <Form form={form} layout="vertical" initialValues={initialValues} onFinish={submit}>
        <Flex gap={12}>
          <Form.Item name="name" label="组名" rules={[{ required: true, message: '请输入组名' }]} style={{ flex: 2 }}>
            <Input maxLength={64} />
          </Form.Item>
          <Form.Item name="type" label="类型" style={{ flex: 2 }}>
            <Select options={typeOptions} />
          </Form.Item>
          <Form.Item
            name="strict"
            label="严格"
            valuePropName="checked"
            tooltip="严格组经任何路径都不能到达 DIRECT（AI 服务等不允许直连的目标）"
            style={{ flex: 1 }}
          >
            <Switch />
          </Form.Item>
        </Flex>

        <Card size="small" title="成员" style={{ marginBottom: 16 }}>
          <Form.Item
            name={['members', 'explicit']}
            label="显式成员（按选择顺序）"
            extra="select 组默认使用第一个成员"
          >
            <Select mode="multiple" showSearch optionFilterProp="label" options={memberOptions} />
          </Form.Item>
          <Form.Item name={['members', 'filter', 'enabled']} label="动态成员（按条件自动包含节点）" valuePropName="checked">
            <Switch />
          </Form.Item>
          {filterOn && (
            <>
              <Typography.Paragraph type="secondary">
                同一项内满足任一即可，各项之间须同时满足；某项留空表示不限。只包含已启用的节点，订阅更新后的新节点会自动加入。
              </Typography.Paragraph>
              <Flex gap={12}>
                <Form.Item name={['members', 'filter', 'regions']} label="地区" style={{ flex: 1 }}>
                  <Select mode="multiple" options={uniq(nodes.map((n) => n.region)).map((r) => ({ value: r, label: r }))} />
                </Form.Item>
                <Form.Item name={['members', 'filter', 'subscriptions']} label="订阅" style={{ flex: 1 }}>
                  <Select
                    mode="multiple"
                    options={subscriptions.map((s) => ({ value: s.id, label: s.display_name || s.name }))}
                  />
                </Form.Item>
              </Flex>
              <Flex gap={12}>
                <Form.Item name={['members', 'filter', 'kinds']} label="节点类型" style={{ flex: 1 }}>
                  <Select mode="multiple" options={kindOptions} />
                </Form.Item>
                <Form.Item name={['members', 'filter', 'protocols']} label="协议" style={{ flex: 1 }}>
                  <Select mode="multiple" options={uniq(nodes.map((n) => n.protocol)).map((p) => ({ value: p, label: p }))} />
                </Form.Item>
              </Flex>
              <Flex gap={12}>
                <Form.Item name={['members', 'filter', 'tags']} label="包含标签" style={{ flex: 1 }}>
                  <Select mode="tags" options={uniq(nodes.flatMap((n) => n.tags)).map((t) => ({ value: t, label: t }))} />
                </Form.Item>
                <Form.Item name={['members', 'filter', 'exclude_tags']} label="排除标签" style={{ flex: 1 }}>
                  <Select mode="tags" options={uniq(nodes.flatMap((n) => n.tags)).map((t) => ({ value: t, label: t }))} />
                </Form.Item>
              </Flex>
              <Form.Item name={['members', 'filter', 'name_regex']} label="名称正则（可选）">
                <Input placeholder="如 家宽|IPLC" />
              </Form.Item>
            </>
          )}
          <Form.Item
            name={['options', 'empty_fallback']}
            label="成员为空时"
            extra="订阅节点全部消失等情况下组内没有成员时使用；默认拒绝（失败即阻断）"
          >
            <Select options={fallbackOptions} />
          </Form.Item>
        </Card>

        {preview && (
          <Alert
            type={preview.used_fallback ? 'warning' : 'info'}
            showIcon
            style={{ marginBottom: 16 }}
            title={preview.used_fallback ? '当前没有匹配的成员，将使用兜底' : `共 ${preview.resolved.length} 个成员`}
            description={
              <>
                <Space size={[4, 4]} wrap>
                  {preview.resolved.slice(0, 60).map((m) => (
                    <Tag key={m.id} color={m.kind === 'group' ? 'purple' : m.kind === 'builtin' ? 'default' : 'cyan'}>
                      {m.name}
                    </Tag>
                  ))}
                  {preview.resolved.length > 60 && <Tag>…还有 {preview.resolved.length - 60} 个</Tag>}
                </Space>
                {preview.warnings.map((w) => (
                  <div key={w}>
                    <Typography.Text type="warning">{w}</Typography.Text>
                  </div>
                ))}
              </>
            }
          />
        )}

        {type !== 'select' && (
          <Card size="small" title="健康检查" style={{ marginBottom: 16 }}>
            <Form.Item name={['options', 'url']} label="测试地址" extra="留空使用 https://www.gstatic.com/generate_204">
              <Input />
            </Form.Item>
            <Space wrap>
              <Form.Item name={['options', 'interval']} label="间隔（秒）">
                <InputNumber min={30} max={86400} placeholder="300" />
              </Form.Item>
              {type === 'url-test' && (
                <Form.Item name={['options', 'tolerance']} label="容差（毫秒）">
                  <InputNumber min={0} max={5000} placeholder="50" />
                </Form.Item>
              )}
              <Form.Item name={['options', 'timeout']} label="超时（毫秒）">
                <InputNumber min={0} max={60000} />
              </Form.Item>
              <Form.Item name={['options', 'lazy']} label="懒惰检查" valuePropName="checked">
                <Switch />
              </Form.Item>
            </Space>
            {type === 'load-balance' && (
              <Form.Item name={['options', 'strategy']} label="策略">
                <Select
                  options={[
                    { value: 'consistent-hashing', label: '散列（consistent-hashing）' },
                    { value: 'round-robin', label: '轮询（round-robin）' },
                    { value: 'sticky-sessions', label: '粘性会话（sticky-sessions）' },
                  ]}
                />
              </Form.Item>
            )}
          </Card>
        )}

        <Collapse
          items={[
            {
              key: 'misc',
              label: '显示',
              forceRender: true,
              children: (
                <Flex gap={12}>
                  <Form.Item name="icon" label="图标 URL" style={{ flex: 3 }}>
                    <Input />
                  </Form.Item>
                  <Form.Item name={['options', 'hidden']} label="在面板中隐藏" valuePropName="checked" style={{ flex: 1 }}>
                    <Switch />
                  </Form.Item>
                </Flex>
              ),
            },
          ]}
        />
      </Form>
    </Modal>
  )
}
