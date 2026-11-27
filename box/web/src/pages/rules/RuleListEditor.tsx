import { useEffect, useRef, useState } from 'react'
import { Button, Checkbox, Empty, Input, Select, type SelectProps, Space, Tag, Tooltip } from 'antd'
import { ArrowDownOutlined, ArrowUpOutlined, DeleteOutlined, HolderOutlined, PlusOutlined } from '@ant-design/icons'
import { RowAction } from '../common'
import {
  type Cond,
  formatRule,
  IP_TYPES,
  LOGIC_TYPES,
  newRule,
  parseRule,
  type Rule,
  ruleError,
  SIMPLE_TYPES,
  TYPE_HINTS,
} from './ruleModel'

const RAW = '原文'
const MATCH = 'MATCH'

type TargetOptions = string[] | { label: string; options: { value: string; label: string }[] }[]

export interface RuleListProps {
  value: string[]
  onChange: (rules: string[]) => void
  targets: TargetOptions
  allowMatch?: boolean // 兜底段：只允许一条 MATCH
  onlyMatch?: boolean
  emptyText?: string
}

type OptionList = NonNullable<SelectProps['options']>
const targetOptions = (t: TargetOptions): OptionList =>
  t.length && typeof t[0] === 'string' ? (t as string[]).map((x) => ({ value: x, label: x })) : (t as OptionList)

// 规则列表编辑器：每种规则类型有表单，逻辑规则可嵌套，拖拽或上下移动调整顺序；解析不了的写法按原文编辑。
// 管理服务的「线路与规则」各分段和路由盒的设备自定义规则共用（ADR-041）。
export default function RuleListEditor({ value, onChange, targets, allowMatch, onlyMatch, emptyText }: RuleListProps) {
  // 内部保留带 key 的结构：父组件只拿到字符串列表。外部值变化（加载、恢复）时重建。
  const [rules, setRules] = useState<Rule[]>(() => value.map(parseRule))
  const last = useRef(value.join('\n'))
  const [dragFrom, setDragFrom] = useState<number>()

  useEffect(() => {
    const joined = value.join('\n')
    if (joined !== last.current) {
      last.current = joined
      setRules(value.map(parseRule))
    }
  }, [value])

  const emit = (next: Rule[]) => {
    setRules(next)
    const lines = next.map(formatRule)
    last.current = lines.join('\n')
    onChange(lines)
  }
  const update = (key: number, patch: Partial<Rule>) => emit(rules.map((r) => (r.key === key ? { ...r, ...patch } : r)))
  const move = (from: number, to: number) => {
    if (to < 0 || to >= rules.length || from === to) return
    const next = [...rules]
    const [r] = next.splice(from, 1)
    next.splice(to, 0, r)
    emit(next)
  }
  const hasMatch = rules.some((r) => r.kind === 'match')
  const opts = targetOptions(targets)

  return (
    <div className="rule-list">
      {rules.length === 0 && <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={emptyText ?? '没有规则'} />}
      {rules.map((r, i) => {
        const err = ruleError(r)
        const kindValue = r.kind === 'raw' ? RAW : r.kind === 'match' ? MATCH : r.cond.kind === 'logic' ? r.cond.op : r.cond.type
        return (
          <div
            key={r.key}
            className={`rule-row${dragFrom === i ? ' dragging' : ''}`}
            draggable
            onDragStart={(e) => {
              setDragFrom(i)
              e.dataTransfer.effectAllowed = 'move'
            }}
            onDragOver={(e) => e.preventDefault()}
            onDrop={(e) => {
              e.preventDefault()
              if (dragFrom !== undefined) move(dragFrom, i)
              setDragFrom(undefined)
            }}
            onDragEnd={() => setDragFrom(undefined)}
          >
            <span className="rule-handle" title="拖动调整顺序">
              <HolderOutlined />
            </span>
            <span className="rule-index">{i + 1}</span>
            <div className="rule-body">
              <Space wrap size={6}>
                <Select
                  size="small"
                  style={{ width: 150 }}
                  value={kindValue}
                  showSearch
                  aria-label="规则类型"
                  disabled={onlyMatch}
                  onChange={(t: string) => {
                    if (t === RAW) update(r.key, { kind: 'raw', raw: formatRule(r) })
                    else if (t === MATCH) update(r.key, { kind: 'match' })
                    else if ((LOGIC_TYPES as readonly string[]).includes(t)) {
                      const first: Cond = r.kind === 'cond' && r.cond.kind === 'simple' ? r.cond : { kind: 'simple', type: 'DOMAIN-SUFFIX', value: '' }
                      const children: Cond[] = t === 'NOT' ? [first] : [first, { kind: 'simple', type: 'NETWORK', value: 'udp' }]
                      update(r.key, { kind: 'cond', cond: { kind: 'logic', op: t as 'AND', children } })
                    } else update(r.key, { kind: 'cond', cond: { kind: 'simple', type: t, value: r.cond.kind === 'simple' ? r.cond.value : '' } })
                  }}
                  options={[
                    ...(onlyMatch ? [] : [{ label: '普通', options: SIMPLE_TYPES.map((x) => ({ value: x, label: x })) }]),
                    ...(onlyMatch ? [] : [{ label: '逻辑', options: LOGIC_TYPES.map((x) => ({ value: x, label: x })) }]),
                    {
                      label: '其他',
                      options: [
                        ...(allowMatch || onlyMatch ? [{ value: MATCH, label: 'MATCH（兜底）', disabled: hasMatch && r.kind !== 'match' }] : []),
                        ...(onlyMatch ? [] : [{ value: RAW, label: '按原文编辑' }]),
                      ],
                    },
                  ]}
                />
                {r.kind === 'raw' ? (
                  <Input
                    size="small"
                    className="mono-input"
                    style={{ width: 420 }}
                    value={r.raw}
                    placeholder="TYPE,值,目标"
                    onChange={(e) => update(r.key, { raw: e.target.value })}
                  />
                ) : r.kind === 'cond' && r.cond.kind === 'simple' ? (
                  <Input
                    size="small"
                    className="mono-input"
                    style={{ width: 220 }}
                    value={r.cond.value}
                    placeholder={TYPE_HINTS[r.cond.type]}
                    onChange={(e) => update(r.key, { cond: { ...(r.cond as Cond & { kind: 'simple' }), value: e.target.value } })}
                  />
                ) : null}
                {r.kind !== 'raw' && (
                  <Select
                    size="small"
                    showSearch
                    style={{ width: 160 }}
                    placeholder="目标"
                    aria-label="目标"
                    value={r.target || undefined}
                    options={opts}
                    onChange={(t: string) => update(r.key, { target: t })}
                  />
                )}
                {r.kind === 'cond' && r.cond.kind === 'simple' && IP_TYPES.has(r.cond.type) && (
                  <Tooltip title="不为了匹配这条规则去解析域名，避免 DNS 泄漏">
                    <Checkbox checked={r.noResolve} onChange={(e) => update(r.key, { noResolve: e.target.checked })}>
                      no-resolve
                    </Checkbox>
                  </Tooltip>
                )}
                {err && <Tag color="error">{err}</Tag>}
              </Space>
              {r.kind === 'cond' && r.cond.kind === 'logic' && (
                <CondEditor cond={r.cond} onChange={(c) => update(r.key, { cond: c })} root />
              )}
            </div>
            <span className="rule-actions">
              <RowAction title="上移" icon={<ArrowUpOutlined />} disabled={i === 0} onClick={() => move(i, i - 1)} />
              <RowAction title="下移" icon={<ArrowDownOutlined />} disabled={i === rules.length - 1} onClick={() => move(i, i + 1)} />
              <RowAction title="删除" icon={<DeleteOutlined />} onClick={() => emit(rules.filter((x) => x.key !== r.key))} />
            </span>
          </div>
        )
      })}
      <Button
        size="small"
        icon={<PlusOutlined />}
        disabled={onlyMatch && hasMatch}
        onClick={() => emit([...rules, onlyMatch ? newRule({ kind: 'match' }) : newRule()])}
      >
        {onlyMatch ? '添加兜底规则' : '添加规则'}
      </Button>
    </div>
  )
}

// CondEditor 编辑逻辑规则的条件，可嵌套。
function CondEditor({ cond, onChange, root }: { cond: Cond; onChange: (c: Cond) => void; root?: boolean }) {
  if (cond.kind === 'simple') {
    return (
      <Space size={6}>
        <Select
          size="small"
          style={{ width: 150 }}
          value={cond.type}
          showSearch
          onChange={(t: string) =>
            (LOGIC_TYPES as readonly string[]).includes(t)
              ? onChange({ kind: 'logic', op: t as 'AND', children: t === 'NOT' ? [cond] : [cond, { kind: 'simple', type: 'NETWORK', value: 'udp' }] })
              : onChange({ ...cond, type: t })
          }
          options={[...SIMPLE_TYPES, ...LOGIC_TYPES].map((x) => ({ value: x, label: x }))}
        />
        <Input size="small" className="mono-input" style={{ width: 200 }} value={cond.value} placeholder={TYPE_HINTS[cond.type]} onChange={(e) => onChange({ ...cond, value: e.target.value })} />
      </Space>
    )
  }
  const setChild = (i: number, c: Cond) => onChange({ ...cond, children: cond.children.map((x, j) => (j === i ? c : x)) })
  return (
    <div className={root ? 'cond-group root' : 'cond-group'}>
      {!root && (
        <Select
          size="small"
          style={{ width: 90, marginBottom: 6 }}
          value={cond.op}
          onChange={(op: 'AND' | 'OR' | 'NOT') => onChange({ ...cond, op })}
          options={LOGIC_TYPES.map((x) => ({ value: x, label: x }))}
        />
      )}
      {cond.children.map((c, i) => (
        <div key={i} className="cond-item">
          <span className="cond-op">{i === 0 ? (cond.op === 'NOT' ? '不满足' : '') : cond.op === 'AND' ? '且' : '或'}</span>
          <CondEditor cond={c} onChange={(x) => setChild(i, x)} />
          {cond.children.length > 1 && (
            <RowAction title="删除条件" icon={<DeleteOutlined />} onClick={() => onChange({ ...cond, children: cond.children.filter((_, j) => j !== i) })} />
          )}
        </div>
      ))}
      {cond.op !== 'NOT' && (
        <Button
          size="small"
          type="link"
          icon={<PlusOutlined />}
          onClick={() => onChange({ ...cond, children: [...cond.children, { kind: 'simple', type: 'DOMAIN-KEYWORD', value: '' }] })}
        >
          添加条件
        </Button>
      )}
    </div>
  )
}
