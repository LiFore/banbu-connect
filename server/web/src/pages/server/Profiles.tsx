import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Alert,
  App,
  Button,
  Card,
  Checkbox,
  Drawer,
  Dropdown,
  Empty,
  Input,
  Modal,
  Popconfirm,
  Radio,
  Segmented,
  Select,
  Skeleton,
  Space,
  Table,
  Tabs,
  Tag,
  Tooltip,
  Typography,
} from 'antd'
import { CopyOutlined, DeleteOutlined, DownOutlined, EditOutlined, HistoryOutlined, ImportOutlined, PlusOutlined, RollbackOutlined, SaveOutlined } from '@ant-design/icons'
import { api, type GroupView, type ProxyNode } from '../../api'
import { serverApi, type Device, type ProfileContent, type ProfileVersion, type RuleProfile, type RuleSet } from '../../serverApi'
import { PageHeader } from '../common'
import { computeScope, type ScopeResult } from './profileScope'
import RulePreviewCard from './RulePreviewCard'
import RuleSegments, { checkSegments, emptySegs, joinSegments, type Segs, splitSegments } from './RuleSegments'
import RuleSetsCard from './RuleSetsCard'

// 方案（ADR-062、ADR-064）：节点、代理组在各自页面维护整个系统的池子；这里决定每个方案拥有哪些节点、代理组，
// 以及线路选择、规则（五段）和远程规则集。「默认」下发给没有分配方案的客户端兜底。
// 内容每次保存记一个版本「方案代码-时间戳」，可以回滚；都要到「发布」后客户端才收到。

type Tab = 'scope' | 'selections' | 'rules' | 'rulesets' | 'preview'

// 可以从其他方案复制的部分；每个页签对应其中几项（规则预览没有可复制的内容）。
type Section = 'nodes' | 'groups' | 'selections' | 'rules' | 'rule_sets'
const SECTIONS: Section[] = ['nodes', 'groups', 'selections', 'rules', 'rule_sets']
const SECTION_LABEL: Record<Section, string> = { nodes: '节点', groups: '代理组', selections: '线路选择', rules: '规则', rule_sets: '远程规则集' }
const TAB_SECTIONS: Record<Tab, Section[]> = {
  scope: ['nodes', 'groups'],
  selections: ['selections'],
  rules: ['rules'],
  rulesets: ['rule_sets'],
  preview: [],
}

// 来源方案这一部分的摘要，复制前给管理员看清楚。
function sectionSummary(p: RuleProfile, k: Section): string {
  switch (k) {
    case 'nodes':
      return p.nodes === null ? '全部' : `${p.nodes.length} 个`
    case 'groups':
      return p.groups === null ? '全部' : `${p.groups.length} 个`
    case 'selections':
      return `${Object.keys(p.selections).length} 条`
    case 'rules':
      return `${p.rules.filter((r) => !r.startsWith('#@segment:')).length} 条`
    case 'rule_sets':
      return p.rule_sets === null ? '客户端内置默认' : `${p.rule_sets.length} 个`
  }
}

const describe = (p: RuleProfile, keys: Section[]) => keys.map((k) => `${SECTION_LABEL[k]}（${sectionSummary(p, k)}）`).join('、')

interface Editing {
  content: ProfileContent
  name: string
  devices: number[]
  segs: Segs
  text: string
  ruleSetList: RuleSet[] // 显示用：rule_sets 为 null 时是内置默认
  ruleSetsCustom: boolean
}

const fromProfile = (p: RuleProfile, defaults: RuleSet[]): Editing => ({
  content: { nodes: p.nodes, groups: p.groups, selections: { ...p.selections }, rules: p.rules, rule_sets: p.rule_sets },
  name: p.name,
  devices: p.devices,
  segs: splitSegments(p.rules),
  text: joinSegments(splitSegments(p.rules)).join('\n'),
  ruleSetList: p.rule_sets ?? defaults,
  ruleSetsCustom: p.rule_sets !== null,
})

export default function Profiles() {
  const { message, modal } = App.useApp()
  const [profiles, setProfiles] = useState<RuleProfile[]>()
  const [devices, setDevices] = useState<Device[]>([])
  const [nodes, setNodes] = useState<ProxyNode[]>([])
  const [groupViews, setGroupViews] = useState<GroupView[]>([])
  const [defaults, setDefaults] = useState<RuleSet[]>([])
  const [selected, setSelected] = useState<number>()
  const [edit, setEdit] = useState<Editing>()
  const [dirty, setDirty] = useState(false)
  const [saving, setSaving] = useState(false)
  const [note, setNote] = useState('')
  const [tab, setTab] = useState<Tab>('scope')
  const [mode, setMode] = useState<'table' | 'text'>('table')
  const [naming, setNaming] = useState<{ id?: number; name: string; copyFrom: number }>()
  const [historyOpen, setHistoryOpen] = useState(false)

  const loadProfiles = useCallback(
    () =>
      Promise.all([serverApi.ruleProfiles(), serverApi.devices()]).then(([p, d]) => {
        setProfiles(p)
        setDevices(d)
        return p
      }),
    [],
  )

  const show = useCallback((p: RuleProfile, defs: RuleSet[]) => {
    setSelected(p.id)
    setEdit(fromProfile(p, defs))
    setDirty(false)
    setNote('')
  }, [])

  useEffect(() => {
    Promise.all([loadProfiles(), api.nodes.list(), api.groups.list(), serverApi.defaultRuleSets()])
      .then(([p, n, g, defs]) => {
        setNodes(n)
        setGroupViews(g)
        setDefaults(defs)
        if (p.length) show(p[0], defs)
      })
      .catch((e: Error) => message.error(e.message))
  }, [loadProfiles, show, message])

  const current = profiles?.find((p) => p.id === selected)
  const active = devices.filter((d) => !d.revoked)
  const ownerOf = (deviceId: number) => profiles?.find((p) => !p.default && p.devices.includes(deviceId))
  const unassigned = active.filter((d) => !ownerOf(d.id)).length

  const confirmDiscard = (go: () => void) => {
    if (!dirty) return go()
    modal.confirm({ title: '放弃还没保存的修改？', okText: '放弃', okButtonProps: { danger: true }, cancelText: '取消', onOk: go })
  }

  const patch = (fn: (e: Editing) => Editing) => {
    setEdit((e) => e && fn(e))
    setDirty(true)
  }

  // 从其他方案复制几部分到正在编辑的内容；保存后才生效。
  const copySections = (src: RuleProfile, keys: Section[]) => {
    patch((e) => {
      const next: Editing = { ...e, content: { ...e.content } }
      for (const k of keys) {
        switch (k) {
          case 'nodes':
            next.content.nodes = src.nodes && [...src.nodes]
            break
          case 'groups':
            next.content.groups = src.groups && [...src.groups]
            break
          case 'selections':
            next.content.selections = { ...src.selections }
            break
          case 'rules': {
            const segs = splitSegments(src.rules)
            next.segs = segs
            next.text = joinSegments(segs).join('\n')
            break
          }
          case 'rule_sets':
            next.ruleSetList = src.rule_sets ? src.rule_sets.map((r) => ({ ...r })) : defaults
            next.ruleSetsCustom = src.rule_sets !== null
            break
        }
      }
      return next
    })
    message.success(`已从「${src.name}」复制${keys.map((k) => SECTION_LABEL[k]).join('、')}，点「保存」后生效`)
  }

  const confirmCopy = (src: RuleProfile, keys: Section[]) =>
    modal.confirm({
      title: `用「${src.name}」的${keys.map((k) => SECTION_LABEL[k]).join('、')}替换当前内容？`,
      content: `来源：${describe(src, keys)}。只改页面上的内容，点「保存」后才生效。`,
      okText: '复制',
      cancelText: '取消',
      onOk: () => copySections(src, keys),
    })
  const [copyAll, setCopyAll] = useState<{ from?: number; keys: Section[] }>()

  const scope: ScopeResult | undefined = useMemo(
    () => (edit ? computeScope(nodes, groupViews, edit.content) : undefined),
    [edit, nodes, groupViews],
  )
  const targets = useMemo(
    () => [
      { label: '方案里的代理组', options: [...(scope?.groups.keys() ?? [])].map((g) => ({ value: g, label: g })) },
      { label: '内置', options: [{ value: 'DIRECT', label: 'DIRECT（直连）' }, { value: 'REJECT', label: 'REJECT（拒绝）' }] },
    ],
    [scope],
  )
  const knownTargets = useMemo(() => new Set(['DIRECT', 'REJECT', ...(scope?.groups.keys() ?? [])]), [scope])

  const switchMode = (m: 'table' | 'text') => {
    if (!edit) return
    if (m === 'text') setEdit({ ...edit, text: joinSegments(edit.segs).join('\n') })
    else setEdit({ ...edit, segs: splitSegments(edit.text.split(/\r?\n/).map((l) => l.trim())) })
    setMode(m)
  }

  const save = async () => {
    if (!edit || !current) return
    const segs = mode === 'text' ? splitSegments(edit.text.split(/\r?\n/).map((l) => l.trim())) : edit.segs
    const err = checkSegments(segs, knownTargets)
    if (err) {
      setTab('rules')
      message.error(err)
      return
    }
    const bad = edit.ruleSetList.find((r) => !r.disabled && !knownTargets.has(r.target))
    if (bad && edit.ruleSetsCustom) message.warning(`规则集「${bad.name}」的目标「${bad.target}」不在这个方案里，下发时会去掉`)
    setSaving(true)
    try {
      const p = await serverApi.updateRuleProfile(current.id, {
        ...edit.content,
        rules: joinSegments(segs),
        rule_sets: edit.ruleSetsCustom ? edit.ruleSetList : null,
        name: edit.name,
        devices: edit.devices,
        note,
      })
      const list = await loadProfiles()
      show(list.find((x) => x.id === p.id) ?? p, defaults)
      message.success(p.version === current.version ? '已保存（内容没变，版本不变）' : `已保存为版本 ${p.version}，到「发布」页发布后客户端才会收到`)
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  const saveName = async () => {
    if (!naming) return
    try {
      let p: RuleProfile
      if (naming.id) {
        const old = profiles!.find((x) => x.id === naming.id)!
        p = await serverApi.updateRuleProfile(old.id, { ...old, name: naming.name })
      } else {
        p = await serverApi.createRuleProfile(naming.name, naming.copyFrom)
      }
      setNaming(undefined)
      const list = await loadProfiles()
      const next = list.find((x) => x.id === p.id) ?? p
      if (naming.id && edit) {
        setEdit({ ...edit, name: next.name })
      } else {
        show(next, defaults)
      }
    } catch (e) {
      message.error((e as Error).message)
    }
  }

  const remove = async (p: RuleProfile) => {
    try {
      await serverApi.deleteRuleProfile(p.id)
      message.success('方案已删除，发布后这些客户端改用默认方案')
      const list = await loadProfiles()
      if (list.length) show(list[0], defaults)
    } catch (e) {
      message.error((e as Error).message)
    }
  }

  const rollback = async (v: ProfileVersion) => {
    if (!current) return
    try {
      const p = await serverApi.rollbackProfile(current.id, v.version)
      const list = await loadProfiles()
      show(list.find((x) => x.id === p.id) ?? p, defaults)
      setHistoryOpen(false)
      message.success(`已回滚到 ${v.version}，新版本 ${p.version}；发布后客户端才会收到`)
    } catch (e) {
      message.error((e as Error).message)
    }
  }

  return (
    <div className="profiles-page">
      <PageHeader
        title="方案"
        count={profiles?.length}
        actions={
          <Space>
            <Button icon={<PlusOutlined />} onClick={() => setNaming({ name: '', copyFrom: profiles?.find((p) => p.default)?.id ?? 0 })}>
              新建方案
            </Button>
            <Input
              style={{ width: 220 }}
              placeholder="版本说明（可选）"
              maxLength={100}
              value={note}
              disabled={!dirty}
              onChange={(e) => setNote(e.target.value)}
            />
            <Button type="primary" icon={<SaveOutlined />} disabled={!dirty} loading={saving} onClick={save}>
              保存
            </Button>
          </Space>
        }
      />
      <Alert
        type="info"
        showIcon
        className="profiles-hint"
        title="节点、代理组在各自页面维护整个系统；方案决定一组客户端拿到其中哪些，以及线路选择、规则和远程规则集。没有分配方案的客户端用「默认」。每次保存记一个版本，可以回滚；都要到「发布」后客户端才收到。"
      />
      {!profiles ? (
        <Skeleton active paragraph={{ rows: 8 }} />
      ) : (
        <div className="profiles-layout">
          <aside className="profile-list" aria-label="方案列表">
            {profiles.map((p) => (
              <button
                key={p.id}
                type="button"
                className={`profile-item${p.id === selected ? ' active' : ''}`}
                onClick={() => p.id !== selected && confirmDiscard(() => show(p, defaults))}
              >
                <span className="profile-item-name">
                  {p.name}
                  {p.id === selected && dirty && <span className="profile-dirty" title="有未保存的修改" />}
                </span>
                <span className="profile-item-meta">
                  {p.default ? `未分配的 ${unassigned} 台` : `${p.devices.length} 台`} · {p.nodes === null ? '全部节点' : `${p.nodes.length} 节点`}
                </span>
                <code className="profile-item-version">{p.version}</code>
              </button>
            ))}
          </aside>
          {current && edit && scope && (
            <section className="profile-detail">
              <Card size="small" className="profile-head">
                <div className="profile-head-row">
                  <Space size={8} wrap>
                    <Typography.Title level={4} style={{ margin: 0 }}>
                      {current.name}
                    </Typography.Title>
                    {current.default && <Tag color="blue">兜底</Tag>}
                    <Typography.Text code copyable={{ text: current.version }} title="当前版本：方案代码-时间戳">
                      {current.version}
                    </Typography.Text>
                  </Space>
                  <Space wrap>
                    <Button icon={<HistoryOutlined />} onClick={() => setHistoryOpen(true)}>
                      版本历史
                    </Button>
                    <Button icon={<ImportOutlined />} disabled={profiles.length < 2} onClick={() => setCopyAll({ keys: [...SECTIONS] })}>
                      从其他方案复制
                    </Button>
                    <Button icon={<CopyOutlined />} onClick={() => setNaming({ name: `${current.name} 副本`, copyFrom: current.id })}>
                      复制为新方案
                    </Button>
                    {!current.default && (
                      <>
                        <Button icon={<EditOutlined />} onClick={() => setNaming({ id: current.id, name: current.name, copyFrom: 0 })}>
                          重命名
                        </Button>
                        <Popconfirm
                          title={`删除方案「${current.name}」？`}
                          description="版本历史一并删除；发布后用这个方案的客户端改用默认方案。"
                          okText="删除"
                          okButtonProps={{ danger: true }}
                          cancelText="取消"
                          onConfirm={() => remove(current)}
                        >
                          <Button danger icon={<DeleteOutlined />}>
                            删除
                          </Button>
                        </Popconfirm>
                      </>
                    )}
                  </Space>
                </div>
                <div className="profile-assign">
                  <span className="profile-assign-label">客户端</span>
                  {current.default ? (
                    <Typography.Text type="secondary">没有分配到其他方案的 {unassigned} 台客户端用这个方案。</Typography.Text>
                  ) : (
                    <Select
                      mode="multiple"
                      allowClear
                      aria-label="使用这个方案的客户端"
                      placeholder="选择使用这个方案的客户端；没有分配时这个方案不下发"
                      style={{ flex: 1, minWidth: 320 }}
                      value={edit.devices}
                      optionFilterProp="label"
                      options={active.map((d) => {
                        const owner = ownerOf(d.id)
                        const other = owner && owner.id !== current.id
                        return { value: d.id, label: other ? `${d.name}（${owner.name}）` : d.name, disabled: !!other }
                      })}
                      onChange={(v: number[]) => patch((e) => ({ ...e, devices: v }))}
                    />
                  )}
                </div>
              </Card>
              <Tabs
                activeKey={tab}
                onChange={(k) => setTab(k as Tab)}
                tabBarExtraContent={
                  TAB_SECTIONS[tab].length > 0 && (
                    <Dropdown
                      disabled={profiles.length < 2}
                      menu={{
                        items: profiles
                          .filter((p) => p.id !== current.id)
                          .map((p) => ({ key: String(p.id), label: `${p.name}：${describe(p, TAB_SECTIONS[tab])}` })),
                        onClick: ({ key }) => {
                          const src = profiles.find((p) => String(p.id) === key)
                          if (src) confirmCopy(src, TAB_SECTIONS[tab])
                        },
                      }}
                    >
                      <Button size="small" icon={<ImportOutlined />}>
                        复制本页自… <DownOutlined />
                      </Button>
                    </Dropdown>
                  )
                }
                items={[
                  {
                    key: 'scope',
                    label: `节点与代理组（${scope.nodes.size} / ${scope.groups.size}）`,
                    children: (
                      <ScopeEditor
                        nodes={nodes}
                        groups={groupViews}
                        content={edit.content}
                        scope={scope}
                        onChange={(c) => patch((e) => ({ ...e, content: { ...e.content, ...c } }))}
                      />
                    ),
                  },
                  {
                    key: 'selections',
                    label: '线路选择',
                    children: (
                      <SelectionsEditor
                        scope={scope}
                        groups={groupViews}
                        selections={edit.content.selections}
                        onChange={(sel) => patch((e) => ({ ...e, content: { ...e.content, selections: sel } }))}
                      />
                    ),
                  },
                  {
                    key: 'rules',
                    label: `规则（${Object.values(edit.segs).reduce((n, l) => n + l.length, 0)}）`,
                    children: (
                      <Card
                        title="规则（从上到下匹配）"
                        extra={
                          <Segmented<'table' | 'text'>
                            value={mode}
                            onChange={switchMode}
                            options={[
                              { label: '分段', value: 'table' },
                              { label: '文本', value: 'text' },
                            ]}
                          />
                        }
                      >
                        <RuleSegments
                          segs={edit.segs}
                          onChange={(segs) => patch((e) => ({ ...e, segs }))}
                          targets={targets}
                          mode={mode}
                          text={edit.text}
                          onText={(text) => patch((e) => ({ ...e, text }))}
                        />
                      </Card>
                    ),
                  },
                  {
                    key: 'rulesets',
                    label: `远程规则集（${edit.ruleSetsCustom ? edit.ruleSetList.length : '内置'}）`,
                    children: (
                      <RuleSetsCard
                        list={edit.ruleSetList}
                        custom={edit.ruleSetsCustom}
                        targets={targets}
                        knownTargets={knownTargets}
                        onChange={(list) => patch((e) => ({ ...e, ruleSetList: list, ruleSetsCustom: true }))}
                        onReset={() => patch((e) => ({ ...e, ruleSetList: defaults, ruleSetsCustom: false }))}
                      />
                    ),
                  },
                  {
                    key: 'preview',
                    label: '规则预览',
                    children: <RulePreviewCard profileId={current.id} draftChanged={dirty} />,
                  },
                ]}
              />
            </section>
          )}
        </div>
      )}
      {current && profiles && (
        <Modal
          title={`从其他方案复制到「${current.name}」`}
          open={!!copyAll}
          okText="复制"
          cancelText="取消"
          okButtonProps={{ disabled: !copyAll?.from || copyAll.keys.length === 0 }}
          onCancel={() => setCopyAll(undefined)}
          onOk={() => {
            const src = profiles.find((p) => p.id === copyAll?.from)
            if (src && copyAll) copySections(src, SECTIONS.filter((k) => copyAll.keys.includes(k)))
            setCopyAll(undefined)
          }}
          destroyOnHidden
        >
          <Space orientation="vertical" size={12} style={{ width: '100%' }}>
            <Select<number>
              style={{ width: '100%' }}
              placeholder="选择来源方案"
              value={copyAll?.from}
              onChange={(from) => setCopyAll((c) => c && { ...c, from })}
              options={profiles.filter((p) => p.id !== current.id).map((p) => ({ value: p.id, label: `${p.name}（${p.version}）` }))}
            />
            <Checkbox.Group<Section>
              style={{ display: 'flex', flexDirection: 'column', gap: 6 }}
              value={copyAll?.keys}
              onChange={(keys) => setCopyAll((c) => c && { ...c, keys })}
              options={SECTIONS.map((k) => {
                const src = profiles.find((p) => p.id === copyAll?.from)
                return { value: k, label: src ? `${SECTION_LABEL[k]}（${sectionSummary(src, k)}）` : SECTION_LABEL[k] }
              })}
            />
            <Typography.Text type="secondary">勾选的部分整份替换当前方案的对应内容；客户端分配和名称不变。只改页面上的内容，点「保存」后记为新版本。</Typography.Text>
          </Space>
        </Modal>
      )}
      <Modal
        title={naming?.id ? '重命名方案' : '新建方案'}
        open={!!naming}
        okText="保存"
        cancelText="取消"
        okButtonProps={{ disabled: !naming?.name.trim() }}
        onOk={saveName}
        onCancel={() => setNaming(undefined)}
        destroyOnHidden
      >
        <Space orientation="vertical" style={{ width: '100%' }}>
          <Input
            autoFocus
            maxLength={40}
            placeholder="方案名称，如：办公、测试机"
            value={naming?.name}
            onChange={(e) => setNaming((n) => n && { ...n, name: e.target.value })}
            onPressEnter={saveName}
          />
          {!naming?.id && (
            <Space>
              <span>内容</span>
              <Select<number>
                style={{ width: 260 }}
                value={naming?.copyFrom}
                onChange={(v) => setNaming((n) => n && { ...n, copyFrom: v })}
                options={[
                  { value: 0, label: '空白（全部节点和代理组，没有规则）' },
                  ...(profiles ?? []).map((p) => ({ value: p.id, label: `复制「${p.name}」当前版本` })),
                ]}
              />
            </Space>
          )}
          {!naming?.id && <Typography.Text type="secondary">版本号自动生成：方案代码（随机、方案唯一）-时间戳。</Typography.Text>}
        </Space>
      </Modal>
      {current && (
        <VersionsDrawer
          open={historyOpen}
          profile={current}
          dirty={dirty}
          onClose={() => setHistoryOpen(false)}
          onRollback={rollback}
        />
      )}
    </div>
  )
}

// 节点与代理组：全部（含以后新增的）或指定；指定时列出系统里的全部，勾选方案拥有的。
function ScopeEditor({
  nodes,
  groups,
  content,
  scope,
  onChange,
}: {
  nodes: ProxyNode[]
  groups: GroupView[]
  content: ProfileContent
  scope: ScopeResult
  onChange: (c: Partial<ProfileContent>) => void
}) {
  const [nodeQ, setNodeQ] = useState('')
  const [nodeSource, setNodeSource] = useState('')
  const enabled = nodes.filter((n) => n.enabled)
  const sources = [...new Set(enabled.map((n) => n.subscription_name || '手动'))]
  const nodeRows = enabled.filter(
    (n) =>
      (!nodeQ || n.display_name.toLowerCase().includes(nodeQ.toLowerCase()) || n.region.includes(nodeQ)) &&
      (!nodeSource || (n.subscription_name || '手动') === nodeSource),
  )
  const pickedNodes = content.nodes ?? []
  const pickedGroups = content.groups ?? []

  return (
    <div className="scope-grid">
      <Card
        size="small"
        title={`节点（下发 ${scope.nodes.size} / ${enabled.length}）`}
        extra={
          <Radio.Group
            size="small"
            optionType="button"
            value={content.nodes === null ? 'all' : 'some'}
            onChange={(e) => onChange({ nodes: e.target.value === 'all' ? null : enabled.map((n) => n.display_name) })}
            options={[
              { value: 'all', label: '全部' },
              { value: 'some', label: '指定' },
            ]}
          />
        }
      >
        {content.nodes === null ? (
          <Typography.Paragraph type="secondary" style={{ margin: 0 }}>
            拥有系统里全部已启用的节点（{enabled.length} 个），以后新增的节点也自动包含。
          </Typography.Paragraph>
        ) : (
          <>
            <Space wrap style={{ marginBottom: 8 }}>
              <Input.Search allowClear size="small" style={{ width: 200 }} placeholder="搜索名称或地区" onChange={(e) => setNodeQ(e.target.value)} />
              <Select
                size="small"
                style={{ width: 160 }}
                value={nodeSource}
                onChange={setNodeSource}
                options={[{ value: '', label: '全部来源' }, ...sources.map((s) => ({ value: s, label: s }))]}
              />
              {scope.autoNodes.length > 0 && (
                <Tooltip title={`选中节点的前置代理：${scope.autoNodes.join('、')}`}>
                  <Tag color="blue">自动带上 {scope.autoNodes.length} 个前置代理</Tag>
                </Tooltip>
              )}
            </Space>
            <Table<ProxyNode>
              rowKey="display_name"
              size="small"
              pagination={false}
              scroll={{ y: 420 }}
              dataSource={nodeRows}
              locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="没有节点" /> }}
              rowSelection={{
                preserveSelectedRowKeys: true,
                selectedRowKeys: pickedNodes,
                onChange: (keys) => onChange({ nodes: keys as string[] }),
              }}
              columns={[
                {
                  title: '名称',
                  dataIndex: 'display_name',
                  render: (v: string) => (
                    <Space size={4}>
                      {v}
                      {scope.autoNodes.includes(v) && <Tag color="blue">前置代理</Tag>}
                    </Space>
                  ),
                },
                { title: '地区', dataIndex: 'region', width: 80 },
                { title: '来源', width: 130, render: (_, n) => n.subscription_name || '手动' },
                { title: '前置代理', dataIndex: 'dialer_name', width: 120, render: (v: string) => v || <span className="hint">—</span> },
              ]}
            />
          </>
        )}
      </Card>
      <Card
        size="small"
        title={`代理组（下发 ${scope.groups.size} / ${groups.length}）`}
        extra={
          <Radio.Group
            size="small"
            optionType="button"
            value={content.groups === null ? 'all' : 'some'}
            onChange={(e) => onChange({ groups: e.target.value === 'all' ? null : groups.map((g) => g.name) })}
            options={[
              { value: 'all', label: '全部' },
              { value: 'some', label: '指定' },
            ]}
          />
        }
      >
        {content.groups === null && (
          <Typography.Paragraph type="secondary">拥有系统里全部代理组，以后新增的也自动包含。成员只保留方案里有的节点和代理组。</Typography.Paragraph>
        )}
        <Table<GroupView>
          rowKey="name"
          size="small"
          pagination={false}
          scroll={{ y: 420 }}
          dataSource={groups}
          rowSelection={
            content.groups === null
              ? undefined
              : { selectedRowKeys: pickedGroups, onChange: (keys) => onChange({ groups: keys as string[] }) }
          }
          columns={[
            {
              title: '名称',
              dataIndex: 'name',
              render: (v: string, g) => (
                <Space size={4}>
                  {v}
                  {g.strict && <Tag color="orange">严格</Tag>}
                </Space>
              ),
            },
            { title: '类型', dataIndex: 'type', width: 100, render: (t: string) => <Tag bordered={false}>{t}</Tag> },
            {
              title: '成员（方案内 / 全部）',
              width: 230,
              render: (_, g) => {
                const kept = scope.groups.get(g.name)
                if (scope.dropped.includes(g.name)) return <Tag color="error">成员都不在方案里，不下发</Tag>
                if (!kept) return <span className="hint">不在方案里</span>
                const removed = scope.trimmed.get(g.name)
                return (
                  <Space size={4}>
                    <span>
                      {kept.length} / {g.resolved.length}
                    </span>
                    {removed && (
                      <Tooltip title={`不在方案里，下发时去掉：${removed.join('、')}`}>
                        <Tag color="warning">去掉 {removed.length} 个</Tag>
                      </Tooltip>
                    )}
                  </Space>
                )
              },
            },
          ]}
        />
      </Card>
    </div>
  )
}

// 线路选择：方案里手动选择类代理组的默认成员，客户端收到新版本时应用一次。
function SelectionsEditor({
  scope,
  groups,
  selections,
  onChange,
}: {
  scope: ScopeResult
  groups: GroupView[]
  selections: Record<string, string>
  onChange: (s: Record<string, string>) => void
}) {
  const lines = groups.filter((g) => g.type === 'select' && scope.groups.has(g.name))
  return (
    <Card title="线路（手动选择类代理组）的默认成员">
      <Typography.Paragraph type="secondary">
        客户端收到新版本时应用一次，之后用户在客户端手动切换不会被覆盖。成员只列方案里有的；新增、编辑线路到「线路」页。
      </Typography.Paragraph>
      <Table<GroupView>
        rowKey="name"
        size="small"
        pagination={false}
        dataSource={lines}
        locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="方案里没有手动选择类代理组" /> }}
        columns={[
          {
            title: '线路',
            dataIndex: 'name',
            render: (v: string, g) => (
              <Space>
                {v}
                {g.strict && <Tag color="orange">严格</Tag>}
              </Space>
            ),
          },
          { title: '成员', width: 90, render: (_, g) => `${scope.groups.get(g.name)?.length ?? 0} 个` },
          {
            title: '默认成员',
            width: 360,
            render: (_, g) => {
              const members = scope.groups.get(g.name) ?? []
              const cur = selections[g.name]
              const missing = cur && !members.includes(cur)
              return (
                <Select
                  allowClear
                  showSearch
                  aria-label={`${g.name} 的默认成员`}
                  placeholder="不指定（客户端保持现状）"
                  style={{ width: 340 }}
                  status={missing ? 'warning' : undefined}
                  value={cur || undefined}
                  options={[
                    ...members.map((m) => ({ label: m, value: m })),
                    ...(missing ? [{ label: `${cur}（不在方案里，不下发）`, value: cur }] : []),
                  ]}
                  onChange={(m?: string) => {
                    const next = { ...selections }
                    if (m) next[g.name] = m
                    else delete next[g.name]
                    onChange(next)
                  }}
                />
              )
            },
          },
        ]}
      />
    </Card>
  )
}

// 版本历史：最新的在前；可以查看每个版本的规则和规则集，回滚会把内容恢复成那个版本并记为新版本（分配和名称不变）。
function VersionsDrawer({
  open,
  profile,
  dirty,
  onClose,
  onRollback,
}: {
  open: boolean
  profile: RuleProfile
  dirty: boolean
  onClose: () => void
  onRollback: (v: ProfileVersion) => void
}) {
  const { message } = App.useApp()
  const [list, setList] = useState<ProfileVersion[]>()

  useEffect(() => {
    if (!open) return
    serverApi.profileVersions(profile.id).then(setList, (e: Error) => message.error(e.message))
  }, [open, profile.id, profile.version, message])

  const all = (n: number) => (n < 0 ? '全部' : String(n))
  return (
    <Drawer title={`「${profile.name}」的版本历史`} open={open} size="large" onClose={onClose} destroyOnHidden>
      {dirty && <Alert style={{ marginBottom: 12 }} type="warning" showIcon title="页面上有还没保存的修改，回滚后会被丢弃。" />}
      <Table<ProfileVersion>
        rowKey="version"
        size="small"
        loading={!list}
        dataSource={list}
        pagination={false}
        expandable={{
          expandedRowRender: (v) => (
            <div className="version-detail">
              <div>
                <b>节点：</b>
                {v.content.nodes === null ? '全部' : v.content.nodes.join('、') || '无'}
              </div>
              <div>
                <b>代理组：</b>
                {v.content.groups === null ? '全部' : v.content.groups.join('、') || '无'}
              </div>
              <div>
                <b>线路选择：</b>
                {Object.entries(v.content.selections).map(([g, m]) => `${g} → ${m}`).join('；') || '未指定'}
              </div>
              <div>
                <b>远程规则集：</b>
                {v.content.rule_sets === null ? '客户端内置默认' : v.content.rule_sets.map((r) => `${r.name}→${r.target}`).join('、') || '无'}
              </div>
              <pre className="version-rules">{splitAndJoin(v.content.rules)}</pre>
            </div>
          ),
        }}
        columns={[
          {
            title: '版本',
            dataIndex: 'version',
            render: (v: string, r) => (
              <Space size={4}>
                <code>{v}</code>
                {r.current && <Tag color="green">当前</Tag>}
              </Space>
            ),
          },
          { title: '时间', dataIndex: 'created_at', width: 170, render: (t: string) => new Date(t).toLocaleString() },
          { title: '说明', dataIndex: 'note', render: (n: string) => n || <span className="hint">—</span> },
          {
            title: '内容',
            width: 220,
            render: (_, v) => (
              <span className="hint">
                节点 {all(v.nodes)} · 组 {all(v.groups)} · 规则 {v.rules} · 规则集 {v.rule_sets < 0 ? '内置' : v.rule_sets}
              </span>
            ),
          },
          {
            title: '',
            width: 90,
            render: (_, v) =>
              v.current ? null : (
                <Popconfirm
                  title={`回滚到 ${v.version}？`}
                  description="内容恢复成这个版本并记为新版本；客户端分配不变，发布后生效。"
                  okText="回滚"
                  cancelText="取消"
                  onConfirm={() => onRollback(v)}
                >
                  <Button size="small" icon={<RollbackOutlined />}>
                    回滚
                  </Button>
                </Popconfirm>
              ),
          },
        ]}
      />
    </Drawer>
  )
}

// 版本里的规则按分段显示成文本。
function splitAndJoin(rules: string[]): string {
  const segs = splitSegments(rules)
  const labels: Record<keyof Segs, string> = { custom: '自定义', service: '服务', direct: '直连', proxy: '代理', fallback: '兜底' }
  const out: string[] = []
  for (const k of Object.keys(emptySegs()) as (keyof Segs)[]) {
    if (segs[k].length) out.push(`# ${labels[k]}`, ...segs[k])
  }
  return out.join('\n') || '（没有规则）'
}
