import { useCallback, useEffect, useMemo, useState } from 'react'
import { Alert, App, Button, Card, Checkbox, Drawer, Empty, Form, Input, InputNumber, Modal, Pagination, Segmented, Select, Space, Switch, Table, Tag, Typography } from 'antd'
import {
  AppstoreAddOutlined,
  ArrowDownOutlined,
  ArrowUpOutlined,
  BarChartOutlined,
  EditOutlined,
  EyeOutlined,
  PlusOutlined,
  ReloadOutlined,
  UndoOutlined,
} from '@ant-design/icons'
import { serverApi, type RuleKind, type RuleSet, type RuleSetPreview, type RuleSetStat } from '../../serverApi'
import { RowAction, RowDelete } from '../common'
import { CATALOG, CATEGORIES, type CatalogCategory, type CatalogSource } from './ruleSetCatalog'

type Targets = { label: string; options: { value: string; label: string }[] }[]

const behaviorLabel: Record<string, string> = { domain: '域名', ipcidr: 'IP 段', classical: '混合' }
const isGeo = (r: RuleSet) => r.type === 'geoip'

// 远程规则集（内置规则）：内核按地址定期下载，命中后交给目标线路。顺序即匹配顺序，排在下发规则之后、兜底规则之前。
// 列表由页面统一保存；「恢复默认」后保存即回到客户端内置的那份。
export default function RuleSetsCard({
  list,
  custom,
  targets,
  knownTargets,
  onChange,
  onReset,
}: {
  list: RuleSet[]
  custom: boolean
  targets: Targets
  knownTargets: Set<string>
  onChange: (list: RuleSet[]) => void
  onReset: () => void
}) {
  const [editing, setEditing] = useState<{ index: number; value: RuleSet }>()
  const [preview, setPreview] = useState<RuleSet>()
  const [catalogOpen, setCatalogOpen] = useState(false)
  const { message } = App.useApp()
  // 条目数、各类别条数和前几个域名（M5-04，ADR-064）：管理服务下载后缓存，打开页面时只读缓存。
  const [stats, setStats] = useState<Record<string, RuleSetStat>>({})
  const [counting, setCounting] = useState(false)
  const statKey = (url?: string, format?: string) => `${url ?? ''}|${format ?? ''}`
  const urls = list.map((r) => statKey(r.url, r.format)).join('\n')

  useEffect(() => {
    if (!urls) return
    serverApi.ruleSetStats(list).then(
      (res) => setStats((cur) => ({ ...cur, ...Object.fromEntries(res.map((x) => [statKey(x.url, x.format), x])) })),
      () => undefined, // 只读缓存，失败不打扰
    )
    // 只在地址变化时重新读缓存
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [urls])

  const countAll = async (refresh: boolean) => {
    setCounting(true)
    try {
      const res = await serverApi.ruleSetStats(list, { fetch: true, refresh })
      setStats((cur) => ({ ...cur, ...Object.fromEntries(res.map((x) => [statKey(x.url, x.format), x])) }))
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setCounting(false)
    }
  }

  const set = (i: number, patch: Partial<RuleSet>) => onChange(list.map((r, j) => (j === i ? { ...r, ...patch } : r)))
  const move = (i: number, d: number) => {
    const next = [...list]
    const [r] = next.splice(i, 1)
    next.splice(i + d, 0, r)
    onChange(next)
  }

  return (
    <Card
      title="远程规则集（从上到下匹配，排在下发规则之后）"
      extra={
        <Space>
          <Button icon={<UndoOutlined />} disabled={!custom} onClick={onReset}>
            恢复默认
          </Button>
          <Button icon={<BarChartOutlined />} loading={counting} onClick={() => countAll(false)} title="下载还没缓存或缓存超过一天的规则集">
            统计条目
          </Button>
          <Button icon={<ReloadOutlined />} loading={counting} onClick={() => countAll(true)} title="全部重新下载并更新缓存">
            全部刷新
          </Button>
          <Button icon={<AppstoreAddOutlined />} onClick={() => setCatalogOpen(true)}>
            从目录添加
          </Button>
          <Button
            icon={<PlusOutlined />}
            onClick={() => setEditing({ index: -1, value: { name: '', behavior: 'domain', url: '', target: '' } })}
          >
            添加规则集
          </Button>
        </Space>
      }
    >
      <Typography.Paragraph type="secondary">
        {custom ? '使用自定义的列表，发布后下发给客户端。' : '当前是内置默认（未下发，客户端用自己的内置列表）；修改并保存、发布后改为下发这里的列表。'}
        规则集由客户端的内核按地址下载，目标线路不存在时这条自动跳过。管理服务下载的内容缓存一天，用于统计、展开查看域名和规则预览；点「全部刷新」重新下载。
      </Typography.Paragraph>
      <Table<RuleSet>
        rowKey={(r) => `${r.type ?? 'rule-set'}:${r.name}`}
        size="small"
        pagination={false}
        dataSource={list}
        scroll={{ x: true }}
        locale={{ emptyText: '没有规则集：除下发规则外，其余流量都走兜底规则' }}
        expandable={{
          rowExpandable: (r) => !isGeo(r) && r.format !== 'mrs' && !!r.url,
          expandedRowRender: (r) => <DomainSample ruleSet={r} stat={stats[statKey(r.url, r.format)]} onMore={() => setPreview(r)} />,
        }}
        columns={[
          { title: '#', width: 44, render: (_, __, i) => i + 1 },
          {
            title: '启用',
            width: 60,
            render: (_, r, i) => <Switch size="small" checked={!r.disabled} onChange={(on) => set(i, { disabled: !on })} />,
          },
          {
            title: '名称',
            render: (_, r) =>
              isGeo(r) ? (
                <Space>
                  <Tag>GEOIP</Tag>
                  {r.name}
                </Space>
              ) : (
                <Space orientation="vertical" size={0}>
                  <Space>
                    <span>{r.name}</span>
                    <Tag>{behaviorLabel[r.behavior ?? ''] ?? r.behavior}</Tag>
                  </Space>
                  <Typography.Text type="secondary" className="selectable" style={{ fontSize: 12, wordBreak: 'break-all' }}>
                    {r.url}
                  </Typography.Text>
                </Space>
              ),
          },
          {
            title: '条目',
            width: 170,
            render: (_, r) => {
              if (isGeo(r)) return <span className="hint">—</span>
              if (r.format === 'mrs') return <span className="hint">二进制</span>
              const st = stats[statKey(r.url, r.format)]
              if (!st || st.missing) return <span className="hint">未统计</span>
              if (st.error && !st.count) return <Tag color="error" title={st.error}>下载失败</Tag>
              return (
                <span title={st.fetched_at ? `缓存于 ${new Date(st.fetched_at).toLocaleString()}${st.error ? `\n${st.error}` : ''}` : undefined}>
                  {st.count} 条
                  <span className="hint"> {kindSummary(st.kinds)}</span>
                </span>
              )
            },
          },
          {
            title: '目标',
            width: 200,
            render: (_, r, i) => {
              const unknown = !knownTargets.has(r.target)
              return (
                <Select
                  showSearch
                  aria-label={`${r.name} 的目标`}
                  style={{ width: 180 }}
                  value={r.target || undefined}
                  status={unknown ? 'warning' : undefined}
                  title={unknown ? '草稿里没有这个代理组，这条会被跳过' : undefined}
                  options={unknown ? [...targets, { label: '不存在', options: [{ value: r.target, label: `${r.target}（不存在）` }] }] : targets}
                  onChange={(target: string) => set(i, { target })}
                />
              )
            },
          },
          {
            title: 'no-resolve',
            width: 90,
            render: (_, r, i) =>
              isGeo(r) || r.behavior !== 'domain' ? (
                <Checkbox aria-label="no-resolve" checked={!!r.no_resolve} onChange={(e) => set(i, { no_resolve: e.target.checked })} />
              ) : null,
          },
          {
            title: '操作',
            width: 170,
            fixed: 'right',
            render: (_, r, i) => (
              <div className="row-actions">
                <RowAction title="查看内容" icon={<EyeOutlined />} disabled={isGeo(r) || r.format === 'mrs'} onClick={() => setPreview(r)} />
                <RowAction title="编辑" icon={<EditOutlined />} onClick={() => setEditing({ index: i, value: r })} />
                <RowAction title="上移" icon={<ArrowUpOutlined />} disabled={i === 0} onClick={() => move(i, -1)} />
                <RowAction title="下移" icon={<ArrowDownOutlined />} disabled={i === list.length - 1} onClick={() => move(i, 1)} />
                <RowDelete confirm={`删除「${r.name}」？`} onConfirm={() => onChange(list.filter((_, j) => j !== i))} />
              </div>
            ),
          },
        ]}
      />
      {editing && (
        <RuleSetForm
          initial={editing.value}
          targets={targets}
          taken={new Set(list.filter((r, j) => j !== editing.index && !isGeo(r)).map((r) => r.name))}
          onCancel={() => setEditing(undefined)}
          onOk={(v) => {
            onChange(editing.index < 0 ? [...list, v] : list.map((r, j) => (j === editing.index ? v : r)))
            setEditing(undefined)
          }}
        />
      )}
      <PreviewDrawer ruleSet={preview} onClose={() => setPreview(undefined)} />
      {catalogOpen && (
        <CatalogModal
          taken={new Set(list.filter((r) => !isGeo(r)).map((r) => r.name))}
          knownTargets={knownTargets}
          targets={targets}
          onCancel={() => setCatalogOpen(false)}
          onOk={(added) => {
            onChange([...list, ...added])
            setCatalogOpen(false)
          }}
        />
      )}
    </Card>
  )
}

function RuleSetForm({
  initial,
  targets,
  taken,
  onOk,
  onCancel,
}: {
  initial: RuleSet
  targets: Targets
  taken: Set<string>
  onOk: (v: RuleSet) => void
  onCancel: () => void
}) {
  const [form] = Form.useForm<RuleSet>()
  const type = Form.useWatch('type', form) ?? initial.type ?? 'rule-set'
  const geo = type === 'geoip'
  return (
    <Modal
      title={initial.name ? `编辑「${initial.name}」` : '添加规则集'}
      open
      okText="确定"
      cancelText="取消"
      onCancel={onCancel}
      onOk={() => form.submit()}
      destroyOnHidden
    >
      <Form<RuleSet>
        form={form}
        layout="vertical"
        initialValues={{ type: 'rule-set', format: '', ...initial }}
        onFinish={(v) => {
          const out: RuleSet = { ...initial, ...v, name: v.name.trim(), target: v.target }
          if (out.type === 'rule-set') delete out.type
          if (geo) {
            delete out.url
            delete out.behavior
            delete out.format
            delete out.interval
          }
          onOk(out)
        }}
      >
        <Form.Item name="type" label="类型">
          <Select
            options={[
              { value: 'rule-set', label: '远程规则集（RULE-SET）' },
              { value: 'geoip', label: 'GEOIP（按 IP 归属地）' },
            ]}
          />
        </Form.Item>
        <Form.Item
          name="name"
          label={geo ? '国家或地区代码' : '名称'}
          extra={geo ? '如 CN、LAN' : '只用字母、数字、_ . -，客户端用它作文件名'}
          rules={[
            { required: true, message: '请填写' },
            { pattern: /^[A-Za-z0-9_.-]{1,64}$/, message: '只能用字母、数字、_ . -' },
            { validator: (_, v: string) => (!geo && taken.has(v?.trim()) ? Promise.reject(new Error('名称重复')) : Promise.resolve()) },
          ]}
        >
          <Input />
        </Form.Item>
        {!geo && (
          <>
            <Form.Item name="url" label="地址" rules={[{ required: true, message: '请填写' }, { pattern: /^https?:\/\/\S+$/, message: '须是 http(s) 链接' }]}>
              <Input className="mono-input" placeholder="https://cdn.jsdelivr.net/gh/…/rules.yaml" />
            </Form.Item>
            <Space style={{ width: '100%' }} align="start">
              <Form.Item name="behavior" label="内容类型" rules={[{ required: true }]}>
                <Select
                  style={{ width: 150 }}
                  options={[
                    { value: 'domain', label: '域名（domain）' },
                    { value: 'ipcidr', label: 'IP 段（ipcidr）' },
                    { value: 'classical', label: '混合（classical）' },
                  ]}
                />
              </Form.Item>
              <Form.Item name="format" label="文件格式">
                <Select
                  style={{ width: 130 }}
                  options={[
                    { value: '', label: 'yaml（默认）' },
                    { value: 'text', label: 'text' },
                    { value: 'mrs', label: 'mrs（二进制）' },
                  ]}
                />
              </Form.Item>
              <Form.Item name="interval" label="更新间隔（秒）">
                <InputNumber min={0} placeholder="86400" style={{ width: 130 }} />
              </Form.Item>
            </Space>
          </>
        )}
        <Form.Item name="target" label="目标" rules={[{ required: true, message: '请选择' }]}>
          <Select showSearch options={targets} placeholder="选择线路" />
        </Form.Item>
        <Form.Item name="no_resolve" valuePropName="checked" extra="IP 类规则建议勾选：不为了匹配规则而解析域名，避免 DNS 泄漏">
          <Checkbox>no-resolve</Checkbox>
        </Form.Item>
      </Form>
    </Modal>
  )
}

const KIND_LABEL: Record<RuleKind, string> = {
  domain: '域名',
  suffix: '后缀',
  keyword: '关键字',
  regex: '通配/正则',
  ip: 'IP',
  other: '其他',
}
const domainCount = (k: RuleSetStat['kinds']) => (k.domain ?? 0) + (k.suffix ?? 0) + (k.keyword ?? 0) + (k.regex ?? 0)

// 「域名 1200 · IP 34」：只列有的类别。
function kindSummary(k: RuleSetStat['kinds']): string {
  const parts: string[] = []
  const d = domainCount(k)
  if (d) parts.push(`域名 ${d}`)
  if (k.ip) parts.push(`IP ${k.ip}`)
  if (k.other) parts.push(`其他 ${k.other}`)
  return parts.length > 1 || (parts.length === 1 && !d) ? parts.join(' · ') : ''
}

// 展开行：显示规则集里的前几个域名，点「查看全部」打开内容抽屉。
function DomainSample({ ruleSet, stat, onMore }: { ruleSet: RuleSet; stat?: RuleSetStat; onMore: () => void }) {
  if (!stat || stat.missing) {
    return (
      <Space>
        <span className="hint">还没有缓存这个规则集的内容。</span>
        <Button size="small" icon={<EyeOutlined />} onClick={onMore}>
          下载并查看
        </Button>
      </Space>
    )
  }
  const d = domainCount(stat.kinds)
  return (
    <div className="domain-sample">
      <span className="hint">{d > 0 ? `域名（共 ${d} 个）：` : `前几条（共 ${stat.count} 条）：`}</span>
      {stat.sample.map((v) => (
        <code key={v} className="domain-chip">
          {v}
        </code>
      ))}
      {stat.count > stat.sample.length && (
        <Button size="small" type="link" onClick={onMore}>
          查看全部
        </Button>
      )}
      {stat.error && <Tag color="warning">{stat.error}</Tag>}
      <span className="hint selectable domain-sample-url">{ruleSet.url}</span>
    </div>
  )
}

function PreviewDrawer({ ruleSet, onClose }: { ruleSet?: RuleSet; onClose: () => void }) {
  return (
    <Drawer title={ruleSet ? `规则集内容：${ruleSet.name}` : ''} open={!!ruleSet} size="large" onClose={onClose} destroyOnHidden>
      {ruleSet && <PreviewBody ruleSet={ruleSet} />}
    </Drawer>
  )
}

type KindFilter = '' | 'domains' | RuleKind
const PAGE = 300

// 规则集内容：按类别（域名、IP 段……）和关键字筛选，服务端分页；域名按网格排列，看得更多。
function PreviewBody({ ruleSet }: { ruleSet: RuleSet }) {
  const [data, setData] = useState<RuleSetPreview>()
  const [error, setError] = useState<string>()
  const [loading, setLoading] = useState(true)
  const [q, setQ] = useState('')
  const [kind, setKind] = useState<KindFilter>('domains')
  const [page, setPage] = useState(1)
  const [raw, setRaw] = useState(false)

  const fetchPage = useCallback(
    (refresh = false) =>
      serverApi
        .previewRuleSet({
          url: ruleSet.url ?? '',
          format: ruleSet.format ?? '',
          behavior: ruleSet.behavior ?? '',
          refresh,
          q,
          kind,
          offset: (page - 1) * PAGE,
          limit: PAGE,
        })
        .then(
          (d) => {
            setData(d)
            setError(undefined)
            setLoading(false)
            // 打开时默认看域名；规则集里没有域名时改看全部。
            if (kind === 'domains' && !q && d.matched === 0 && d.count > 0) setKind('')
          },
          (e: Error) => {
            setError(e.message)
            setLoading(false)
          },
        ),
    [ruleSet.url, ruleSet.format, ruleSet.behavior, q, kind, page],
  )
  useEffect(() => {
    void fetchPage()
  }, [fetchPage])
  const reload = (refresh: boolean) => {
    setLoading(true)
    void fetchPage(refresh)
  }

  const kinds = data?.kinds ?? {}
  const domainTotal = domainCount(kinds)
  const kindOptions = [
    { label: `域名 ${domainTotal}`, value: 'domains' as KindFilter, disabled: domainTotal === 0 },
    { label: `IP ${kinds.ip ?? 0}`, value: 'ip' as KindFilter, disabled: !kinds.ip },
    { label: `其他 ${kinds.other ?? 0}`, value: 'other' as KindFilter, disabled: !kinds.other },
    { label: `全部 ${data?.count ?? 0}`, value: '' as KindFilter },
  ]

  return (
    <Space orientation="vertical" size={12} style={{ width: '100%' }}>
      <Typography.Text type="secondary" className="selectable" style={{ wordBreak: 'break-all' }}>
        {ruleSet.url}
      </Typography.Text>
      {error && (
        <Alert
          type="error"
          showIcon
          title="下载或解析失败"
          description={error}
          action={
            <Button size="small" onClick={() => reload(true)}>
              重试
            </Button>
          }
        />
      )}
      {data?.warning && <Alert type="warning" showIcon title={data.warning} />}
      {data && (
        <Space wrap>
          <Tag color="blue">{data.count} 条</Tag>
          <Tag>{(data.bytes / 1024).toFixed(1)} KB</Tag>
          <Typography.Text type="secondary">
            {data.cached ? '缓存于' : '下载于'} {new Date(data.fetched_at).toLocaleString()}
          </Typography.Text>
          <Button size="small" icon={<ReloadOutlined />} loading={loading} onClick={() => reload(true)}>
            重新下载
          </Button>
        </Space>
      )}
      <Space wrap>
        <Segmented<KindFilter>
          value={kind}
          options={kindOptions}
          onChange={(k) => {
            setKind(k)
            setPage(1)
          }}
        />
        <Input.Search
          allowClear
          style={{ width: 260 }}
          placeholder="搜索域名或规则"
          onSearch={(v) => {
            setQ(v.trim())
            setPage(1)
          }}
        />
        <Checkbox checked={raw} onChange={(e) => setRaw(e.target.checked)}>
          显示原文
        </Checkbox>
      </Space>
      {data && kind === 'domains' && domainTotal > 0 && (
        <Space size={[4, 4]} wrap>
          {(['domain', 'suffix', 'keyword', 'regex'] as RuleKind[])
            .filter((k) => kinds[k])
            .map((k) => (
              <Tag key={k}>
                {KIND_LABEL[k]} {kinds[k]}
              </Tag>
            ))}
        </Space>
      )}
      {loading && !data ? (
        <Table loading size="small" dataSource={[]} columns={[]} />
      ) : data && data.items.length === 0 ? (
        <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={q ? '没有匹配的条目' : '没有条目'} />
      ) : (
        <div className={`domain-grid${raw ? ' raw' : ''}`}>
          {data?.items.map((it, i) => (
            <div key={`${(page - 1) * PAGE + i}`} className="domain-cell" title={it.rule}>
              {!raw && it.kind !== 'domain' && <span className={`domain-kind kind-${it.kind}`}>{KIND_LABEL[it.kind]}</span>}
              <code className="selectable">{raw ? it.rule : it.value}</code>
            </div>
          ))}
        </div>
      )}
      {data && data.matched > PAGE && (
        <Pagination
          size="small"
          current={page}
          pageSize={PAGE}
          total={data.matched}
          showSizeChanger={false}
          showTotal={(n) => `${n} 条`}
          onChange={setPage}
        />
      )}
    </Space>
  )
}

// 从目录添加：按分类、来源筛选和搜索，勾选条目；名称已存在的不能重复添加（规则集名称唯一）。
function CatalogModal({
  taken,
  knownTargets,
  targets,
  onOk,
  onCancel,
}: {
  taken: Set<string>
  knownTargets: Set<string>
  targets: Targets
  onOk: (added: RuleSet[]) => void
  onCancel: () => void
}) {
  const [picked, setPicked] = useState<Record<string, string>>({}) // 名称 → 目标
  const [category, setCategory] = useState<CatalogCategory | ''>('')
  const [source, setSource] = useState<CatalogSource | ''>('')
  const [q, setQ] = useState('')
  const rows = useMemo(() => {
    const k = q.trim().toLowerCase()
    return CATALOG.filter(
      (c) =>
        (!category || c.category === category) &&
        (!source || c.source === source) &&
        (!k || c.label.toLowerCase().includes(k) || c.name.includes(k)),
    ).map((c) => ({ ...c, exists: taken.has(c.name) }))
  }, [category, source, q, taken])
  const valid = Object.entries(picked).every(([, t]) => t)
  return (
    <Modal
      title={`从目录添加规则集（${CATALOG.length} 个）`}
      open
      width={900}
      okText={`添加 ${Object.keys(picked).length} 个`}
      okButtonProps={{ disabled: Object.keys(picked).length === 0 || !valid }}
      onCancel={onCancel}
      onOk={() => onOk(CATALOG.filter((c) => c.name in picked).map((c) => ({ ...c.ruleSet, target: picked[c.name] })))}
      destroyOnHidden
    >
      <Space wrap style={{ marginBottom: 12 }}>
        <Segmented<CatalogCategory | ''>
          value={category}
          onChange={setCategory}
          options={[{ label: '全部', value: '' }, ...CATEGORIES.map((c) => ({ label: c, value: c }))]}
        />
        <Select<CatalogSource | ''>
          style={{ width: 150 }}
          value={source}
          onChange={setSource}
          options={[
            { value: '', label: '全部来源' },
            { value: 'Loyalsoldier', label: 'Loyalsoldier' },
            { value: 'blackmatrix7', label: 'blackmatrix7' },
            { value: 'MetaCubeX', label: 'MetaCubeX' },
          ]}
        />
        <Input.Search allowClear style={{ width: 200 }} placeholder="搜索名称" onChange={(e) => setQ(e.target.value)} />
        {Object.keys(picked).length > 0 && <Tag color="green">已选 {Object.keys(picked).length} 个</Tag>}
      </Space>
      <Table
        rowKey="name"
        size="small"
        pagination={false}
        scroll={{ y: 460 }}
        dataSource={rows}
        rowSelection={{
          preserveSelectedRowKeys: true,
          selectedRowKeys: Object.keys(picked),
          getCheckboxProps: (r) => ({ disabled: r.exists }),
          onChange: (keys) =>
            setPicked((cur) =>
              Object.fromEntries(
                keys.map((k) => {
                  const c = CATALOG.find((x) => x.name === k)
                  const suggested = c?.target && knownTargets.has(c.target) ? c.target : ''
                  return [k as string, cur[k as string] ?? suggested]
                }),
              ),
            ),
        }}
        columns={[
          {
            title: '规则集',
            render: (_, r) => (
              <Space size={4} wrap>
                {r.label}
                <Tag>{r.category}</Tag>
                <Tag bordered={false}>{r.source}</Tag>
                <Tag bordered={false}>{behaviorLabel[r.ruleSet.behavior ?? ''] ?? r.ruleSet.behavior}</Tag>
                {r.large && (
                  <Tag color="warning" title="条目很多，客户端内存占用较大">
                    大
                  </Tag>
                )}
                {r.exists && <Tag color="default">已添加</Tag>}
              </Space>
            ),
          },
          { title: '名称', dataIndex: 'name', width: 190, render: (v: string) => <code>{v}</code> },
          {
            title: '目标',
            width: 200,
            render: (_, r) =>
              r.name in picked ? (
                <Select
                  size="small"
                  showSearch
                  style={{ width: 180 }}
                  placeholder="选择目标"
                  value={picked[r.name] || undefined}
                  options={targets}
                  onChange={(t: string) => setPicked((p) => ({ ...p, [r.name]: t }))}
                />
              ) : null,
          },
        ]}
      />
    </Modal>
  )
}
