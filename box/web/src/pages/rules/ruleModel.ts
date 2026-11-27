// Mihomo 规则的结构化表示（M5-01、M5-02）：普通规则、可嵌套的逻辑规则（AND/OR/NOT）、MATCH，解析不了的按原文。

export const SIMPLE_TYPES = [
  'DOMAIN-SUFFIX',
  'DOMAIN',
  'DOMAIN-KEYWORD',
  'DOMAIN-REGEX',
  'GEOSITE',
  'IP-CIDR',
  'IP-CIDR6',
  'IP-SUFFIX',
  'IP-ASN',
  'GEOIP',
  'SRC-IP-CIDR',
  'DST-PORT',
  'SRC-PORT',
  'PROCESS-NAME',
  'PROCESS-PATH',
  'NETWORK',
  'RULE-SET',
] as const
export const LOGIC_TYPES = ['AND', 'OR', 'NOT'] as const
// 这些类型可以加 no-resolve：不为了匹配规则去解析域名。
export const IP_TYPES = new Set(['IP-CIDR', 'IP-CIDR6', 'IP-SUFFIX', 'IP-ASN', 'GEOIP', 'RULE-SET'])

export const TYPE_HINTS: Record<string, string> = {
  'DOMAIN-SUFFIX': 'example.com（含子域名）',
  DOMAIN: 'www.example.com（完整域名）',
  'DOMAIN-KEYWORD': 'google',
  'DOMAIN-REGEX': '^ad\\..*',
  GEOSITE: 'cn、google',
  'IP-CIDR': '1.2.3.0/24',
  'IP-CIDR6': '2001:db8::/32',
  'IP-SUFFIX': '8.8.8.8/24',
  'IP-ASN': '13335',
  GEOIP: 'CN、LAN',
  'SRC-IP-CIDR': '192.168.1.10/32',
  'DST-PORT': '443 或 8000-9000',
  'SRC-PORT': '12345',
  'PROCESS-NAME': 'curl',
  'PROCESS-PATH': '/usr/bin/curl',
  NETWORK: 'tcp 或 udp',
  'RULE-SET': '规则集名称',
}

export interface SimpleCond {
  kind: 'simple'
  type: string
  value: string
}

export interface LogicCond {
  kind: 'logic'
  op: 'AND' | 'OR' | 'NOT'
  children: Cond[]
}

export type Cond = SimpleCond | LogicCond

export interface Rule {
  key: number
  kind: 'cond' | 'match' | 'raw'
  cond: Cond
  target: string
  noResolve: boolean
  raw: string
}

let nextKey = 1
export const newRule = (r: Partial<Rule> = {}): Rule => ({
  key: nextKey++,
  kind: 'cond',
  cond: { kind: 'simple', type: 'DOMAIN-SUFFIX', value: '' },
  target: '',
  noResolve: false,
  raw: '',
  ...r,
})

// splitTop 按最外层的逗号切分（括号内的逗号不算）。
function splitTop(s: string): string[] | undefined {
  const out: string[] = []
  let depth = 0
  let cur = ''
  for (const ch of s) {
    if (ch === '(') depth++
    if (ch === ')') depth--
    if (depth < 0) return undefined
    if (ch === ',' && depth === 0) {
      out.push(cur)
      cur = ''
    } else cur += ch
  }
  if (depth !== 0) return undefined
  out.push(cur)
  return out
}

const unwrap = (s: string) => (s.startsWith('(') && s.endsWith(')') ? s.slice(1, -1) : undefined)

// parseCond 解析逻辑规则里的一个条件，如 DOMAIN,a.com 或 AND,((DOMAIN,a),(NETWORK,udp))。
function parseCond(s: string): Cond | undefined {
  const parts = splitTop(s.trim())
  if (!parts || parts.length < 2) return undefined
  const type = parts[0].trim().toUpperCase()
  if ((LOGIC_TYPES as readonly string[]).includes(type)) {
    if (parts.length !== 2) return undefined
    const inner = unwrap(parts[1].trim())
    const items = inner === undefined ? undefined : splitTop(inner)
    if (!items) return undefined
    const children: Cond[] = []
    for (const it of items) {
      const body = unwrap(it.trim())
      const c = body === undefined ? undefined : parseCond(body)
      if (!c) return undefined
      children.push(c)
    }
    return { kind: 'logic', op: type as LogicCond['op'], children }
  }
  if (parts.length !== 2) return undefined
  return { kind: 'simple', type, value: parts[1].trim() }
}

export function parseRule(line: string): Rule {
  const t = line.trim()
  const parts = splitTop(t)
  if (!parts || parts.length < 2) return newRule({ kind: 'raw', raw: t })
  const type = parts[0].trim().toUpperCase()
  if (type === 'MATCH' && parts.length === 2) return newRule({ kind: 'match', target: parts[1].trim() })
  if ((LOGIC_TYPES as readonly string[]).includes(type) && parts.length === 3) {
    const cond = parseCond(`${parts[0]},${parts[1]}`)
    if (cond) return newRule({ cond, target: parts[2].trim() })
  }
  if ((SIMPLE_TYPES as readonly string[]).includes(type) && parts.length >= 3) {
    const extras = parts.slice(3).map((x) => x.trim())
    if (extras.every((x) => x === 'no-resolve')) {
      return newRule({ cond: { kind: 'simple', type, value: parts[1].trim() }, target: parts[2].trim(), noResolve: extras.length > 0 })
    }
  }
  return newRule({ kind: 'raw', raw: t })
}

export function formatCond(c: Cond): string {
  if (c.kind === 'simple') return `${c.type},${c.value.trim()}`
  return `${c.op},(${c.children.map((x) => `(${formatCond(x)})`).join(',')})`
}

export function formatRule(r: Rule): string {
  if (r.kind === 'raw') return r.raw.trim()
  if (r.kind === 'match') return `MATCH,${r.target.trim()}`
  const base = `${formatCond(r.cond)},${r.target.trim()}`
  return r.cond.kind === 'simple' && r.noResolve && IP_TYPES.has(r.cond.type) ? `${base},no-resolve` : base
}

function condError(c: Cond): string | undefined {
  if (c.kind === 'simple') {
    if (!c.value.trim()) return '缺少匹配内容'
    if (/[,()]/.test(c.value)) return '匹配内容不能含逗号或括号'
    return undefined
  }
  if (c.op === 'NOT' && c.children.length !== 1) return 'NOT 只能有一个条件'
  if (c.op !== 'NOT' && c.children.length < 2) return `${c.op} 至少要两个条件`
  for (const x of c.children) {
    const e = condError(x)
    if (e) return e
  }
  return undefined
}

export function ruleError(r: Rule): string | undefined {
  if (r.kind === 'raw') return r.raw.trim().includes(',') ? undefined : '格式不对'
  if (!r.target.trim()) return '缺少目标'
  if (r.kind === 'match') return undefined
  return condError(r.cond)
}

export const toRules = (lines: string[]) =>
  lines
    .map((l) => l.trim())
    .filter((l) => l && !l.startsWith('#'))
    .map(parseRule)
