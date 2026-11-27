import { useState } from 'react'
import { Alert, Checkbox, Collapse, Descriptions, List, Modal, Space, Statistic, Tag, Typography } from 'antd'
import { api, type NodeRef, type Preview, type Subscription } from '../../api'

interface Props {
  subscription: Subscription
  preview: Preview
  onClose: () => void
  onApplied: () => void
}

const kindLabel = { group: '代理组', node: '节点前置代理', rule: '规则' } as const

function Counts({ data }: { data: Record<string, number> }) {
  return (
    <Space wrap size={[4, 4]}>
      {Object.entries(data)
        .sort((a, b) => b[1] - a[1])
        .map(([k, v]) => (
          <Tag key={k}>
            {k} × {v}
          </Tag>
        ))}
    </Space>
  )
}

function NameList({ items }: { items: string[] }) {
  return <List size="small" dataSource={items} renderItem={(s) => <List.Item>{s}</List.Item>} />
}

function NodeList({ items }: { items: NodeRef[] }) {
  return (
    <List
      size="small"
      dataSource={items}
      renderItem={(n) => (
        <List.Item>
          {n.display_name} <Tag>{n.protocol}</Tag>
        </List.Item>
      )}
    />
  )
}

// 订阅更新预览（M2-10）：先看差异与影响，确认后再写入。
export default function PreviewModal({ subscription, preview, onClose, onApplied }: Props) {
  const { report, diff, impacts, conflicts } = preview
  const [confirmed, setConfirmed] = useState(false)
  const [applying, setApplying] = useState(false)
  const [error, setError] = useState<string>()
  const needConfirm = impacts.length > 0

  const apply = async () => {
    setApplying(true)
    setError(undefined)
    try {
      await api.subscriptions.apply(subscription.id, preview.preview_id, confirmed)
      onApplied()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setApplying(false)
    }
  }

  const sections = [
    { key: 'added', label: `新增 ${diff.added.length}`, children: <NodeList items={diff.added} />, show: diff.added.length },
    { key: 'removed', label: `删除 ${diff.removed.length}`, children: <NodeList items={diff.removed} />, show: diff.removed.length },
    {
      key: 'changed',
      label: `变更 ${diff.changed.length}`,
      show: diff.changed.length,
      children: (
        <List
          size="small"
          dataSource={diff.changed}
          renderItem={(n) => (
            <List.Item>
              {n.display_name}：{n.fields.join('、')}
            </List.Item>
          )}
        />
      ),
    },
    { key: 'filtered', label: `信息节点（已过滤）${report.filtered.length}`, children: <NameList items={report.filtered} />, show: report.filtered.length },
    { key: 'unmatched', label: `地区无法识别（已丢弃）${report.unmatched.length}`, children: <NameList items={report.unmatched} />, show: report.unmatched.length },
    { key: 'duplicates', label: `重复节点（已去重）${report.duplicates.length}`, children: <NameList items={report.duplicates} />, show: report.duplicates.length },
    {
      key: 'invalid',
      label: `校验失败 ${report.invalid.length}`,
      show: report.invalid.length,
      children: <NameList items={report.invalid.map((i) => `${i.name}：${i.error}`)} />,
    },
    {
      key: 'conflicts',
      label: `与其它订阅重复（已跳过）${conflicts.length}`,
      show: conflicts.length,
      children: <NameList items={conflicts.map((c) => `${c.display_name}（已在 ${c.owner_subscription}）`)} />,
    },
  ].filter((s) => s.show)

  return (
    <Modal
      title={`更新预览：${subscription.display_name || subscription.name}`}
      open
      width={720}
      onCancel={onClose}
      onOk={apply}
      okText="应用"
      okButtonProps={{ disabled: needConfirm && !confirmed }}
      confirmLoading={applying}
    >
      {error && <Alert type="error" showIcon title={error} style={{ marginBottom: 16 }} />}
      <Space size="large" style={{ marginBottom: 16 }}>
        <Statistic title="解析" value={report.parsed} />
        <Statistic title="保留" value={report.kept} />
        <Statistic title="新增" value={diff.added.length} />
        <Statistic title="删除" value={diff.removed.length} />
        <Statistic title="变更" value={diff.changed.length} />
        <Statistic title="无变化" value={diff.unchanged} />
      </Space>
      <Descriptions column={1} size="small" style={{ marginBottom: 16 }}>
        <Descriptions.Item label="格式">{preview.format === 'clash' ? 'Clash / Mihomo' : '分享链接'}</Descriptions.Item>
        <Descriptions.Item label="地区">
          <Counts data={report.regions} />
        </Descriptions.Item>
        <Descriptions.Item label="协议">
          <Counts data={report.protocols} />
        </Descriptions.Item>
      </Descriptions>

      {needConfirm && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          title={`将删除的节点仍被 ${impacts.length} 处引用`}
          description={
            <>
              <List
                size="small"
                dataSource={impacts}
                renderItem={(i) => (
                  <List.Item>
                    {i.node_name} ← {kindLabel[i.kind]}「{i.ref_name || i.ref_id}」
                  </List.Item>
                )}
              />
              <Checkbox checked={confirmed} onChange={(e) => setConfirmed(e.target.checked)}>
                我已了解：仍然删除这些节点，上述引用将失效，需要我到对应位置调整
              </Checkbox>
            </>
          }
        />
      )}
      {sections.length > 0 ? (
        <Collapse items={sections.map(({ key, label, children }) => ({ key, label, children }))} />
      ) : (
        <Typography.Text type="secondary">没有变化。</Typography.Text>
      )}
    </Modal>
  )
}
