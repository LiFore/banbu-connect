import type { ReactNode } from 'react'
import { Button, Empty, Popconfirm, Skeleton, Tooltip, Typography } from 'antd'
import { DeleteOutlined } from '@ant-design/icons'

// 页头：左侧标题和数量，右侧操作，主操作放最右。
export function PageHeader({ title, count, actions }: { title: string; count?: number; actions?: ReactNode }) {
  return (
    <div className="page-header">
      <Typography.Title level={3} className="page-title">
        {title}
        {count !== undefined && <span className="page-count">{count}</span>}
      </Typography.Title>
      {actions && <div className="page-actions">{actions}</div>}
    </div>
  )
}

// 表格行内操作：只显示图标，名称放在提示里，所有行都对齐。
export function RowAction({
  title,
  icon,
  onClick,
  loading,
  disabled,
}: {
  title: string
  icon: ReactNode
  onClick?: () => void
  loading?: boolean
  disabled?: boolean
}) {
  return (
    <Tooltip title={title}>
      <Button size="small" type="text" aria-label={title} icon={icon} loading={loading} disabled={disabled} onClick={onClick} />
    </Tooltip>
  )
}

// 删除一律先确认，确认按钮写明动作，不用笼统的“确定”。
export function RowDelete({ confirm, onConfirm }: { confirm: string; onConfirm: () => void }) {
  return (
    <Popconfirm title={confirm} okText="删除" okButtonProps={{ danger: true }} cancelText="取消" onConfirm={onConfirm}>
      <span>
        <Tooltip title="删除">
          <Button size="small" type="text" danger aria-label="删除" icon={<DeleteOutlined />} />
        </Tooltip>
      </span>
    </Popconfirm>
  )
}

// 表格的空状态：加载中显示骨架，确实没有数据才显示提示。
export function tableEmpty(loading: boolean, text: string, action?: ReactNode) {
  return loading ? (
    <Skeleton active title={false} paragraph={{ rows: 3 }} />
  ) : (
    <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={text}>
      {action}
    </Empty>
  )
}

// 字节数显示成 KB / MB / GB。
export function formatBytes(n: number): string {
  if (!n || n < 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v >= 100 || i === 0 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`
}
