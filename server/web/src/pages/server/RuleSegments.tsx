import { useState } from 'react'
import { Input, Segmented, Tag, Typography } from 'antd'
import RuleListEditor from '../rules/RuleListEditor'
import { ruleError, toRules } from '../rules/ruleModel'

// 规则分段（M5-03）：顺序固定，段内拖拽排序；兜底段只能有一条 MATCH。与 internal/dist 的 segments 对应。
export const SEGMENTS = [
  { key: 'custom', label: '自定义', hint: '优先级最高，放临时或特殊的规则' },
  { key: 'service', label: '服务', hint: '按服务分流，如 Claude、ChatGPT、流媒体' },
  { key: 'direct', label: '直连', hint: '国内服务、局域网等直连' },
  { key: 'proxy', label: '代理', hint: '需要代理的境外服务' },
  { key: 'fallback', label: '兜底', hint: '只能有一条 MATCH；不写时客户端用「漏网之鱼」组，没有该组则直连。放在远程规则集之后' },
] as const
export type SegKey = (typeof SEGMENTS)[number]['key']
export type Segs = Record<SegKey, string[]>
export const MARK = '#@segment:'

export const emptySegs = (): Segs => ({ custom: [], service: [], direct: [], proxy: [], fallback: [] })

// 没有分段标记的旧规则都归入「自定义」。
export function splitSegments(lines: string[]): Segs {
  const out = emptySegs()
  let cur: SegKey = 'custom'
  for (const l of lines) {
    if (l.startsWith(MARK)) {
      const k = l.slice(MARK.length) as SegKey
      if (k in out) cur = k
      continue
    }
    out[cur].push(l)
  }
  return out
}

export const joinSegments = (s: Segs) => SEGMENTS.flatMap((g) => [MARK + g.key, ...s[g.key]])

// 检查规则：格式和目标（目标须是方案里的代理组或内置出站）。返回错误提示，没有问题时返回 undefined。
export function checkSegments(segs: Segs, knownTargets: Set<string>): string | undefined {
  for (const g of SEGMENTS) {
    const rules = toRules(segs[g.key])
    const bad = rules.findIndex((r) => ruleError(r))
    if (bad >= 0) return `「${g.label}」段第 ${bad + 1} 条规则${ruleError(rules[bad])}`
    const unknown = rules.find((r) => r.kind !== 'raw' && r.target && !knownTargets.has(r.target))
    if (unknown) return `「${g.label}」段的目标「${unknown.target}」不在这个方案里`
  }
  return undefined
}

type Targets = { label: string; options: { value: string; label: string }[] }[]
type Columns = 'auto' | '1' | '2' | '3'
const COLS_KEY = 'banbu.ruleColumns'

function loadCols(): Columns {
  try {
    const v = localStorage.getItem(COLS_KEY)
    if (v === '1' || v === '2' || v === '3' || v === 'auto') return v
  } catch {
    /* 浏览器存储不可用时用默认 */
  }
  return 'auto'
}

// 分段规则编辑：分段（表单）或文本两种模式；分段模式可分一、二、三栏排布，「自动」按页面宽度决定。
// 文本模式的内容由父组件持有：切换模式时互相转换。
export default function RuleSegments({
  segs,
  onChange,
  targets,
  mode,
  text,
  onText,
}: {
  segs: Segs
  onChange: (s: Segs) => void
  targets: Targets
  mode: 'table' | 'text'
  text: string
  onText: (t: string) => void
}) {
  const [cols, setCols] = useState<Columns>(loadCols)
  const pickCols = (c: Columns) => {
    setCols(c)
    try {
      localStorage.setItem(COLS_KEY, c)
    } catch {
      /* 忽略 */
    }
  }

  if (mode === 'text') {
    return (
      <>
        <Typography.Paragraph type="secondary">
          每行一条；<code>{MARK}段名</code> 是分段标记（custom、service、direct、proxy、fallback），# 开头的其他行为注释（保存时去掉）。
        </Typography.Paragraph>
        <Input.TextArea
          className="mono-input"
          autoSize={{ minRows: 6, maxRows: 32 }}
          spellCheck={false}
          value={text}
          onChange={(e) => onText(e.target.value)}
        />
      </>
    )
  }
  return (
    <>
      <div className="rule-columns-bar">
        <Typography.Text type="secondary">
          匹配顺序：设备规则 → 自定义 → 服务 → 直连 → 代理 → 受保护服务 → 远程规则集 → 兜底。下发规则排在客户端本地规则之前。
        </Typography.Text>
        <Segmented<Columns>
          size="small"
          value={cols}
          onChange={pickCols}
          options={[
            { label: '自动', value: 'auto' },
            { label: '一栏', value: '1' },
            { label: '两栏', value: '2' },
            { label: '三栏', value: '3' },
          ]}
        />
      </div>
      <div className={`rule-segments cols-${cols}`}>
        {SEGMENTS.map((g) => (
          <section key={g.key} className={`rule-segment seg-${g.key}`}>
            <div className="rule-segment-head">
              <span className="rule-segment-title">{g.label}</span>
              <Tag>{segs[g.key].length}</Tag>
              <span className="hint">{g.hint}</span>
            </div>
            <RuleListEditor
              value={segs[g.key]}
              targets={targets}
              onlyMatch={g.key === 'fallback'}
              emptyText={g.key === 'fallback' ? '不设置时用客户端默认兜底' : '这一段还没有规则'}
              onChange={(rules) => onChange({ ...segs, [g.key]: rules })}
            />
          </section>
        ))}
      </div>
    </>
  )
}
