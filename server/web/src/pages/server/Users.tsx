import { useCallback, useEffect, useState } from 'react'
import { App, Form, Input, Modal, Select, Space, Table, Tag, Typography } from 'antd'
import { EditOutlined, PlusOutlined } from '@ant-design/icons'
import { Button } from 'antd'
import { serverApi, type Device, type User, type UserInput } from '../../serverApi'
import { PageHeader, RowAction, RowDelete, tableEmpty } from '../common'
import { deviceOptions } from './assign'

// 用户：有姓名，名下可以有多台设备（一台设备只属于一个用户）。账号可以分配给用户，名下所有设备都能用。
export default function Users() {
  const { message } = App.useApp()
  const [users, setUsers] = useState<User[]>([])
  const [devices, setDevices] = useState<Device[]>([])
  const [loading, setLoading] = useState(true)
  const [editing, setEditing] = useState<User | 'new'>()
  const [saving, setSaving] = useState(false)
  const [form] = Form.useForm<UserInput>()

  const reload = useCallback(
    () =>
      Promise.all([serverApi.users(), serverApi.devices()])
        .then(([u, d]) => {
          setUsers(u)
          setDevices(d)
        })
        .catch((e: Error) => message.error(e.message))
        .finally(() => setLoading(false)),
    [message],
  )
  useEffect(() => {
    void reload()
  }, [reload])

  const open = (u: User | 'new') => {
    setEditing(u)
    form.setFieldsValue(u === 'new' ? { name: '', note: '', devices: [] } : { name: u.name, note: u.note, devices: u.devices })
  }

  const save = async () => {
    const v = await form.validateFields()
    setSaving(true)
    try {
      if (editing === 'new') await serverApi.createUser(v)
      else if (editing) await serverApi.updateUser(editing.id, v)
      message.success('已保存')
      setEditing(undefined)
      await reload()
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  const remove = (u: User) =>
    serverApi
      .deleteUser(u.id)
      .then(() => message.success('已删除'))
      .catch((e: Error) => message.error(e.message))
      .finally(reload)

  const deviceName = (id: number) => devices.find((d) => d.id === id)?.name ?? `#${id}`
  // 选设备时，已经属于别的用户的，标出现在的归属：选中后转到这个用户名下。
  const options = deviceOptions(devices, users)

  return (
    <Space orientation="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader
        title="用户"
        count={users.length}
        actions={
          <Button type="primary" icon={<PlusOutlined />} onClick={() => open('new')}>
            新建
          </Button>
        }
      />
      <Table<User>
        rowKey="id"
        size="small"
        dataSource={users}
        pagination={false}
        locale={{ emptyText: tableEmpty(loading, '还没有用户') }}
        columns={[
          { title: '姓名', dataIndex: 'name', width: 200 },
          {
            title: '设备',
            dataIndex: 'devices',
            render: (v: number[]) =>
              v.length === 0 ? (
                <Typography.Text type="secondary">没有设备</Typography.Text>
              ) : (
                <Space size={[4, 4]} wrap>
                  {v.map((id) => (
                    <Tag key={id}>{deviceName(id)}</Tag>
                  ))}
                </Space>
              ),
          },
          { title: '备注', dataIndex: 'note', render: (v: string) => v || '—' },
          {
            title: '操作',
            width: 120,
            render: (_, u) => (
              <Space size={0}>
                <RowAction title="编辑" icon={<EditOutlined />} onClick={() => open(u)} />
                <RowDelete confirm={`删除「${u.name}」？名下的设备不会删除，但分配给这个用户的账号对这些设备不再生效。`} onConfirm={() => remove(u)} />
              </Space>
            ),
          },
        ]}
      />
      <Modal
        title={editing === 'new' ? '新建用户' : '编辑用户'}
        open={!!editing}
        okText="保存"
        cancelText="取消"
        confirmLoading={saving}
        onOk={save}
        onCancel={() => setEditing(undefined)}
        destroyOnHidden
      >
        <Form form={form} layout="vertical" preserve={false}>
          <Form.Item name="name" label="姓名" rules={[{ required: true, whitespace: true, message: '请填写姓名' }]}>
            <Input maxLength={32} autoFocus />
          </Form.Item>
          <Form.Item name="devices" label="名下的设备" extra="一台设备只属于一个用户：选中已属于别人的设备会转到这个用户名下。">
            <Select mode="multiple" allowClear placeholder="选择设备" optionFilterProp="label" options={options} />
          </Form.Item>
          <Form.Item name="note" label="备注">
            <Input maxLength={200} />
          </Form.Item>
        </Form>
      </Modal>
    </Space>
  )
}
