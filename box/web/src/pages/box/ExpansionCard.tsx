import { Alert, Card, Empty, Table, Tag, Tooltip } from 'antd'
import { NodeExpandOutlined } from '@ant-design/icons'
import type { ExpandedRuleSet } from '../../api'

// 目标为严格组的规则集：其中的域名和网段也进 guard 的保护范围（ADR-039）。
export default function ExpansionCard({ value }: { value?: { rule_sets: ExpandedRuleSet[]; truncated?: boolean } }) {
  const list = value?.rule_sets ?? []
  return (
    <Card
      title={
        <span className="card-title">
          <NodeExpandOutlined />
          规则集展开
        </span>
      }
    >
      {value?.truncated && <Alert type="warning" showIcon title="条目过多，只取了前 20000 条" style={{ marginBottom: 12 }} />}
      <Table<ExpandedRuleSet>
        rowKey="name"
        size="small"
        pagination={false}
        dataSource={list}
        locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="没有指向严格组的规则集" /> }}
        columns={[
          { title: '规则集', dataIndex: 'name', render: (n: string, r) => <Tooltip title={r.url}>{n}</Tooltip> },
          { title: '代理组', dataIndex: 'group' },
          { title: '域名', render: (_, r) => r.suffixes.length },
          { title: '网段', render: (_, r) => r.cidrs.length },
          {
            title: '无法展开',
            render: (_, r) =>
              r.skipped > 0 ? (
                <Tooltip title={`如 ${(r.samples ?? []).join('、')}：GEOSITE、关键字等只在内核里生效，内核不在时不受保护`}>
                  <Tag color="warning">{r.skipped}</Tag>
                </Tooltip>
              ) : (
                0
              ),
          },
          {
            title: '状态',
            render: (_, r) =>
              r.error ? (
                <Tooltip title={`${r.error}；沿用上一次的结果`}>
                  <Tag color="error">下载失败</Tag>
                </Tooltip>
              ) : (
                <Tag color="success">{r.updated_at ? new Date(r.updated_at).toLocaleString() : '—'}</Tag>
              ),
          },
        ]}
      />
      <p className="hint">规则集指向严格代理组时，其中的域名和网段同样不允许直连；每 6 小时或规则集变化时重新下载，失败时沿用上一次的结果。</p>
    </Card>
  )
}
