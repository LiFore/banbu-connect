import { Collapse, Tag, Typography } from 'antd'
import { DesktopOutlined, UserOutlined } from '@ant-design/icons'
import type { Device, User } from '../../serverApi'

// 账号分配：账号可以分配给设备，也可以分配给用户（用户名下所有设备都能用）。
// 生效的设备 = 直接分配的设备 + 分配的用户名下的设备。设备是主要对象，用户只作辅助说明。

/** 设备下拉选项：设备名为主，所属用户放在括号里辅助。only 不为空时只列这些设备。 */
export function deviceOptions(devices: Device[], users: User[], only?: number[]) {
  return devices
    .filter((d) => !d.revoked && (!only || only.includes(d.id)))
    .map((d) => {
      const u = users.find((x) => x.id === d.user_id)
      return { value: d.id, label: u ? `${d.name}（${u.name}）` : d.name }
    })
}

/** 用户下拉选项：姓名加名下的设备数。 */
export function userOptions(users: User[], devices: Device[]) {
  return users.map((u) => {
    const n = devices.filter((d) => !d.revoked && d.user_id === u.id).length
    return { value: u.id, label: `${u.name}（${n} 台设备）` }
  })
}

/** 编辑时预览生效的设备：直接选的加上选中用户名下的。 */
export function effectiveDevices(direct: number[] | undefined, userIds: number[] | undefined, users: User[]): number[] {
  const out = [...(direct ?? [])]
  for (const uid of userIds ?? []) {
    for (const d of users.find((u) => u.id === uid)?.devices ?? []) if (!out.includes(d)) out.push(d)
  }
  return out
}

interface AssignedProps {
  devices: Device[]
  users: User[]
  effective: number[] // 生效的设备
  direct: number[] // 直接分配的设备
  userIds: number[] // 分配给的用户
  holder?: number // Codex：Refresh Token 持有者
}

/** 账号卡片下方折叠的分配明细：每台生效的设备（主）和它所属的用户（辅），以及分配的用户。 */
export function AssignedList({ devices, users, effective, direct, userIds, holder }: AssignedProps) {
  const device = (id: number) => devices.find((d) => d.id === id)
  const user = (id: number) => users.find((u) => u.id === id)
  const assigned = userIds.map(user).filter((u): u is User => !!u)
  if (effective.length === 0 && assigned.length === 0) return null
  return (
    <Collapse
      ghost
      size="small"
      className="ai-assign"
      items={[
        {
          key: 'assigned',
          label: (
            <span className="ai-assign-title">
              <DesktopOutlined /> {effective.length} 台设备
              {assigned.length > 0 && (
                <>
                  {' · '}
                  <UserOutlined /> {assigned.length} 位用户
                </>
              )}
            </span>
          ),
          children: (
            <div className="ai-assign-body">
              {effective.length > 0 && (
                <ul className="ai-assign-list">
                  {effective.map((id) => {
                    const d = device(id)
                    const owner = d && user(d.user_id)
                    const viaUser = !direct.includes(id) && owner && userIds.includes(owner.id)
                    return (
                      <li key={id}>
                        <span className="ai-assign-main">{d?.name ?? `#${id}`}</span>
                        {owner && (
                          <Typography.Text type="secondary" className="ai-assign-sub">
                            {owner.name}
                          </Typography.Text>
                        )}
                        {viaUser ? <Tag bordered={false}>经用户</Tag> : <Tag bordered={false}>直接分配</Tag>}
                        {holder === id && <Tag color="gold">Refresh Token 持有者</Tag>}
                      </li>
                    )
                  })}
                </ul>
              )}
              {assigned.length > 0 && (
                <div className="ai-assign-users">
                  <Typography.Text type="secondary">分配给用户：</Typography.Text>
                  {assigned.map((u) => (
                    <Tag key={u.id} icon={<UserOutlined />}>
                      {u.name}
                      <span className="hint">（{u.devices.length} 台设备）</span>
                    </Tag>
                  ))}
                </div>
              )}
            </div>
          ),
        },
      ]}
    />
  )
}
