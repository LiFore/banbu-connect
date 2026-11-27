import type { EChartsCoreOption } from 'echarts/core'
import type { RankItem, Topology, TrendPoint, UsageSeries } from '../../../serverApi'
import { formatBytes } from '../../common'
import { themeColor } from './EChart'

// 遥测页各图表的配置。颜色取 antd 主题色，图例和坐标轴保持克制。

const brand = () => themeColor('--ant-color-primary', '#15803d')
const accent = () => themeColor('--ant-color-warning', '#f59e0b')
const muted = () => themeColor('--ant-color-text-quaternary', '#c0c4c9')

export const bytesOf = (t: { upload: number; download: number }) => t.upload + t.download

function timeLabel(t: string, step: string) {
  const d = new Date(t)
  if (step === 'day') return d.toLocaleDateString([], { month: 'numeric', day: 'numeric' })
  if (step === 'hour') return d.toLocaleString([], { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' })
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
}

// 流量趋势：上传、下载堆叠面积，未归属单独一条虚线。
export function trendOption(points: TrendPoint[], step: string, unattributed = true): EChartsCoreOption {
  const x = points.map((p) => timeLabel(p.time, step))
  const series: object[] = [
    { name: '下载', type: 'line', stack: 'proxy', areaStyle: { opacity: 0.25 }, showSymbol: false, data: points.map((p) => p.download), color: brand() },
    { name: '上传', type: 'line', stack: 'proxy', areaStyle: { opacity: 0.25 }, showSymbol: false, data: points.map((p) => p.upload), color: accent() },
  ]
  if (unattributed) {
    series.push({ name: '未归属', type: 'line', showSymbol: false, lineStyle: { type: 'dashed' }, data: points.map((p) => p.unattributed), color: muted() })
  }
  return {
    grid: { left: 64, right: 16, top: 32, bottom: 28 },
    legend: { top: 0, right: 0 },
    tooltip: { trigger: 'axis', valueFormatter: (v: number) => formatBytes(v) },
    xAxis: { type: 'category', data: x, boundaryGap: false },
    yAxis: { type: 'value', axisLabel: { formatter: (v: number) => formatBytes(v) } },
    series,
  }
}

// 排行：横向柱状图，最大的在上面。
export function rankOption(items: RankItem[]): EChartsCoreOption {
  const rows = [...items].reverse()
  return {
    grid: { left: 8, right: 64, top: 4, bottom: 4, containLabel: true },
    tooltip: {
      trigger: 'item',
      formatter: (p: { dataIndex: number }) => {
        const it = rows[p.dataIndex]
        return `${it.label}<br/>流量 ${formatBytes(bytesOf(it))}（↑${formatBytes(it.upload)} ↓${formatBytes(it.download)}）<br/>连接 ${it.conns}`
      },
    },
    xAxis: { type: 'value', show: false },
    yAxis: {
      type: 'category',
      data: rows.map((r) => r.label),
      axisLabel: { width: 160, overflow: 'truncate' },
      axisTick: { show: false },
    },
    series: [
      {
        type: 'bar',
        data: rows.map(bytesOf),
        color: brand(),
        barMaxWidth: 14,
        label: { show: true, position: 'right', formatter: (p: { value: number }) => formatBytes(p.value) },
      },
    ],
  }
}

// 拓扑：客户端 → 链路各跳节点 → 目标地址，连线粗细为流量或连接数。
export function sankeyOption(t: Topology, metric: 'bytes' | 'conns'): EChartsCoreOption {
  const weight = (x: { upload: number; download: number; conns: number }) => (metric === 'bytes' ? bytesOf(x) : x.conns)
  const kindColor = { client: brand(), hop: accent(), host: themeColor('--ant-color-info', '#1677ff') }
  const maxDepth = Math.max(0, ...t.nodes.filter((n) => n.kind === 'hop').map((n) => n.depth))
  return {
    tooltip: {
      trigger: 'item',
      formatter: (p: { dataType: string; data: { name?: string; source?: string; target?: string; raw: { upload: number; download: number; conns: number } } }) => {
        const r = p.data.raw
        const title = p.dataType === 'edge' ? `${nameOf(t, p.data.source)} → ${nameOf(t, p.data.target)}` : p.data.name
        return `${title}<br/>流量 ${formatBytes(bytesOf(r))}<br/>连接 ${r.conns}`
      },
    },
    series: [
      {
        type: 'sankey',
        left: 8,
        right: 160,
        top: 8,
        bottom: 8,
        nodeGap: 10,
        nodeAlign: 'justify',
        draggable: false,
        emphasis: { focus: 'adjacency' },
        lineStyle: { color: 'gradient', opacity: 0.35, curveness: 0.5 },
        label: { formatter: (p: { data: { label: string } }) => p.data.label },
        data: t.nodes.map((n) => ({
          name: n.id,
          label: n.name,
          raw: n,
          depth: n.kind === 'host' ? maxDepth + 1 : n.depth,
          itemStyle: { color: kindColor[n.kind] },
        })),
        links: t.links.filter((l) => weight(l) > 0).map((l) => ({ source: l.source, target: l.target, value: Math.max(weight(l), 1), raw: l })),
      },
    ],
  }
}

function nameOf(t: Topology, id?: string) {
  return t.nodes.find((n) => n.id === id)?.name ?? id ?? ''
}

// 用量：每个客户端一组堆叠柱，最多 8 个，其余合并为「其他」。
export function usageOption(series: UsageSeries[], step: 'hour' | 'day'): EChartsCoreOption {
  const times = Array.from(new Set(series.flatMap((s) => s.points.map((p) => p.time)))).sort()
  const top = series.slice(0, 8)
  const rest = series.slice(8)
  const line = (s: { label: string; points: TrendPoint[] }) => {
    const by = new Map<string, number>()
    for (const p of s.points) by.set(p.time, (by.get(p.time) ?? 0) + bytesOf(p))
    return { name: s.label, type: 'bar', stack: 'usage', data: times.map((t) => by.get(t) ?? 0), barMaxWidth: 24 }
  }
  const data = top.map(line)
  if (rest.length) data.push(line({ label: '其他', points: rest.flatMap((s) => s.points) }))
  return {
    grid: { left: 64, right: 16, top: 40, bottom: 28 },
    legend: { top: 0, type: 'scroll' },
    tooltip: { trigger: 'axis', valueFormatter: (v: number) => formatBytes(v) },
    xAxis: { type: 'category', data: times.map((t) => timeLabel(t, step)) },
    yAxis: { type: 'value', axisLabel: { formatter: (v: number) => formatBytes(v) } },
    series: data,
  }
}
