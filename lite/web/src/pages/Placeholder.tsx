import { Empty, Typography } from 'antd'

export default function Placeholder({ title, features }: { title: string; features: string }) {
  return (
    <>
      <Typography.Title level={3}>{title}</Typography.Title>
      <Empty description={features ? `待实现：功能清单 ${features}` : '没有这个页面'} />
    </>
  )
}
