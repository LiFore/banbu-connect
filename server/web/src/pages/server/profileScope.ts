import type { GroupView, ProxyNode } from '../../api'
import type { ProfileContent } from '../../serverApi'

// 按方案筛选节点和代理组（ADR-064），与 internal/dist/rule_profiles.go 的 scope 对应，用于页面上的提示和可选目标：
//   - 选中节点的前置代理自动带上；
//   - 代理组成员只保留方案里有的节点、代理组和内置出站；筛选后没有成员的组去掉（本来就为空的不动）。
export interface ScopeResult {
  nodes: Set<string> // 最终下发的节点（含自动带上的前置代理）
  groups: Map<string, string[]> // 最终下发的代理组 → 成员
  autoNodes: string[] // 因前置代理自动带上的节点
  dropped: string[] // 选了但筛选后没有成员、被去掉的组
  trimmed: Map<string, string[]> // 选了但部分成员不在方案里的组 → 被去掉的成员
}

const BUILTIN = new Set(['DIRECT', 'REJECT', 'REJECT-DROP', 'PASS', 'COMPATIBLE'])

export function computeScope(nodes: ProxyNode[], groups: GroupView[], c: Pick<ProfileContent, 'nodes' | 'groups'>): ScopeResult {
  const enabled = nodes.filter((n) => n.enabled)
  const nodeByName = new Map(enabled.map((n) => [n.display_name, n]))
  const groupByName = new Map(groups.map((g) => [g.name, g]))
  const inNodes = new Set(enabled.map((n) => n.display_name).filter((n) => c.nodes === null || c.nodes.includes(n)))
  const inGroups = new Set(groups.map((g) => g.name).filter((g) => c.groups === null || c.groups.includes(g)))
  const picked = new Set(inNodes)
  const autoNodes: string[] = []
  for (let changed = true; changed; ) {
    changed = false
    for (const name of [...inNodes]) {
      const d = nodeByName.get(name)?.dialer_name
      if (!d) continue
      if (nodeByName.has(d) && !inNodes.has(d)) {
        inNodes.add(d)
        if (!picked.has(d)) autoNodes.push(d)
        changed = true
      }
      if (groupByName.has(d) && !inGroups.has(d)) {
        inGroups.add(d)
        changed = true
      }
    }
  }
  const members = new Map<string, string[]>()
  const dropped: string[] = []
  for (let changed = true; changed; ) {
    changed = false
    for (const name of [...inGroups]) {
      const all = groupByName.get(name)!.resolved.map((m) => m.name)
      const kept = all.filter((m) => inNodes.has(m) || inGroups.has(m) || BUILTIN.has(m))
      if (kept.length === 0 && all.length > 0) {
        inGroups.delete(name)
        dropped.push(name)
        changed = true
        continue
      }
      members.set(name, kept)
    }
  }
  const out = new Map<string, string[]>()
  const trimmed = new Map<string, string[]>()
  for (const g of groups) {
    if (!inGroups.has(g.name)) continue
    const kept = members.get(g.name) ?? []
    out.set(g.name, kept)
    const removed = g.resolved.map((m) => m.name).filter((m) => !kept.includes(m))
    if (removed.length) trimmed.set(g.name, removed)
  }
  return { nodes: inNodes, groups: out, autoNodes, dropped, trimmed }
}
