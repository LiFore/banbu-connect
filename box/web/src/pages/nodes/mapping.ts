import type { Mapping, ProtocolSchema } from '../../api'

// 按 "a.b.c" 路径读写嵌套配置。

export function getPath(m: Mapping, key: string): unknown {
  let cur: unknown = m
  for (const k of key.split('.')) {
    if (cur === null || typeof cur !== 'object') return undefined
    cur = (cur as Mapping)[k]
  }
  return cur
}

function isEmpty(v: unknown): boolean {
  return v === undefined || v === null || v === '' || (Array.isArray(v) && v.length === 0)
}

/** setPath 写入值；值为空时删除该键，并清理随之变空的父对象。 */
export function setPath(m: Mapping, key: string, value: unknown): void {
  const parts = key.split('.')
  const parents: Mapping[] = [m]
  let cur = m
  for (const k of parts.slice(0, -1)) {
    if (typeof cur[k] !== 'object' || cur[k] === null) {
      if (isEmpty(value)) return
      cur[k] = {}
    }
    cur = cur[k] as Mapping
    parents.push(cur)
  }
  const last = parts[parts.length - 1]
  if (isEmpty(value)) {
    delete cur[last]
    for (let i = parts.length - 2; i >= 0; i--) {
      if (Object.keys(parents[i + 1]).length === 0) delete parents[i][parts[i]]
    }
  } else {
    cur[last] = value
  }
}

/**
 * fromForm 以原配置为底，覆盖表单中 schema 字段的值（保留表单未覆盖的导入字段）。
 * 表单值以 schema key 为扁平键存放，避免 antd 把 "ws-opts.headers.Host" 当作路径。
 */
export function fromForm(base: Mapping, type: string, schema: ProtocolSchema, values: Record<string, unknown>): Mapping {
  const m: Mapping = structuredClone(base)
  m.type = type
  for (const f of schema.fields) setPath(m, f.key, values[f.key])
  return m
}

export function toForm(m: Mapping, schema: ProtocolSchema): Record<string, unknown> {
  const values: Record<string, unknown> = {}
  for (const f of schema.fields) {
    const v = getPath(m, f.key)
    values[f.key] = v === undefined ? f.default : v
  }
  return values
}

export function defaults(schema: ProtocolSchema): Record<string, unknown> {
  return Object.fromEntries(schema.fields.map((f) => [f.key, f.default]))
}
