import { useState } from 'react'
import { Alert, Button, Input, Modal, Select, Space, Table, Tag, Typography } from 'antd'
import { api, type GroupView, type ImportCandidate, type ProxyNode } from '../../api'

interface Props {
  allNodes: ProxyNode[]
  groups: GroupView[]
  onClose: () => void
  onSaved: () => void
}

interface Row extends ImportCandidate {
  key: number
  status?: 'saved' | string // 保存结果：saved 或错误信息
}

const placeholder = `支持以下任意格式，可多行混合：
vless://… / hy2://… / anytls://… / trojan://… / ss://… / tuic://…
curl --proxy gw.example.com:33335 --proxy-user USER:PASS https://…
gw.example.com:22225:USER:PASS
http://USER:PASS@host:port   socks5://USER:PASS@host:port
或 Mihomo YAML 片段（单个节点或 proxies: 列表）`

// 粘贴导入（M3-03/04/05）：先解析预览，确认后逐条保存。
export default function ImportModal({ allNodes, groups, onClose, onSaved }: Props) {
  const [text, setText] = useState('')
  const [rows, setRows] = useState<Row[]>([])
  const [selected, setSelected] = useState<number[]>([])
  const [dialer, setDialer] = useState('')
  const [parsing, setParsing] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string>()

  const parse = async () => {
    setParsing(true)
    setError(undefined)
    try {
      const cands = await api.nodes.import(text)
      const rs = cands.map((c, i) => ({ ...c, key: i }))
      setRows(rs)
      setSelected(rs.filter((r) => !r.error).map((r) => r.key))
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setParsing(false)
    }
  }

  const save = async () => {
    setSaving(true)
    const next = [...rows]
    for (const key of selected) {
      const r = next[key]
      if (r.error || r.status === 'saved' || !r.mapping) continue
      try {
        await api.nodes.create({
          kind: r.kind,
          display_name: r.display_name,
          region: '',
          mapping: r.mapping,
          dialer_ref: dialer,
          tags: [],
        })
        next[key] = { ...r, status: 'saved' }
      } catch (e) {
        next[key] = { ...r, status: (e as Error).message }
      }
    }
    setRows(next)
    setSaving(false)
    if (selected.every((k) => next[k].status === 'saved')) onSaved()
  }

  const rename = (key: number, name: string) =>
    setRows((rs) => rs.map((r) => (r.key === key ? { ...r, display_name: name, status: undefined } : r)))

  return (
    <Modal
      title="粘贴导入节点"
      open
      width={820}
      onCancel={onClose}
      footer={
        <Space>
          <Button onClick={onClose}>关闭</Button>
          <Button onClick={parse} loading={parsing} disabled={!text.trim()}>
            解析
          </Button>
          <Button type="primary" onClick={save} loading={saving} disabled={selected.length === 0}>
            保存选中（{selected.length}）
          </Button>
        </Space>
      }
    >
      {error && <Alert type="error" showIcon title={error} style={{ marginBottom: 16 }} />}
      <Input.TextArea
        rows={6}
        value={text}
        onChange={(e) => setText(e.target.value)}
        placeholder={placeholder}
        style={{ fontFamily: 'monospace', marginBottom: 16 }}
      />
      {rows.length > 0 && (
        <>
          <Space style={{ marginBottom: 8 }}>
            <Typography.Text>前置代理（应用到本次保存的全部节点）：</Typography.Text>
            <Select
              style={{ width: 320 }}
              showSearch
              optionFilterProp="label"
              value={dialer}
              onChange={setDialer}
              options={[
                { label: '直连', options: [{ value: '', label: '（不使用前置代理）' }] },
                { label: '代理组', options: groups.map((g) => ({ value: g.id, label: g.name })) },
                { label: '节点', options: allNodes.map((n) => ({ value: n.id, label: n.display_name })) },
              ]}
            />
          </Space>
          <Table<Row>
            size="small"
            rowKey="key"
            pagination={false}
            dataSource={rows}
            rowSelection={{
              selectedRowKeys: selected,
              onChange: (keys) => setSelected(keys as number[]),
              getCheckboxProps: (r) => ({ disabled: !!r.error || r.status === 'saved' }),
            }}
            columns={[
              {
                title: '名称',
                render: (_, r) =>
                  r.error ? (
                    <Typography.Text type="secondary">—</Typography.Text>
                  ) : (
                    <Input size="small" value={r.display_name} onChange={(e) => rename(r.key, e.target.value)} />
                  ),
              },
              {
                title: '类型',
                width: 90,
                render: (_, r) => <Tag color={r.kind === 'isp' ? 'gold' : 'cyan'}>{r.kind === 'isp' ? 'ISP' : '自建'}</Tag>,
              },
              { title: '地址', dataIndex: 'summary' },
              {
                title: '状态',
                render: (_, r) =>
                  r.error ? (
                    <Typography.Text type="danger">{r.error}</Typography.Text>
                  ) : r.status === 'saved' ? (
                    <Tag color="success">已保存</Tag>
                  ) : r.status ? (
                    <Typography.Text type="danger">{r.status}</Typography.Text>
                  ) : r.warning ? (
                    <Typography.Text type="warning">{r.warning}</Typography.Text>
                  ) : (
                    <Tag>待保存</Tag>
                  ),
              },
            ]}
          />
        </>
      )}
    </Modal>
  )
}
