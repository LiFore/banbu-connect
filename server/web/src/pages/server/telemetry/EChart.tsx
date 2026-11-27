import { useEffect, useRef } from 'react'
import * as echarts from 'echarts/core'
import { BarChart, LineChart, SankeyChart } from 'echarts/charts'
import { GridComponent, LegendComponent, TooltipComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'
import type { EChartsCoreOption } from 'echarts/core'

// 遥测页的图表（ADR-058）：ECharts 按需引入折线、柱状、桑基图，只有遥测页（懒加载）用到，不进其他页面的包。
echarts.use([LineChart, BarChart, SankeyChart, GridComponent, LegendComponent, TooltipComponent, CanvasRenderer])

export type ChartClick = { dataType?: string; data?: unknown; name?: string; dataIndex?: number; seriesIndex?: number }

// themeColor 读 antd 的主题色（Canvas 读不了 CSS 变量，只能运行时取一次）。
export function themeColor(name: string, fallback: string): string {
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim()
  return v || fallback
}

export default function EChart({ option, height, onClick }: { option: EChartsCoreOption; height: number; onClick?: (p: ChartClick) => void }) {
  const ref = useRef<HTMLDivElement>(null)
  const chart = useRef<echarts.ECharts>(undefined)
  const click = useRef(onClick)

  useEffect(() => {
    click.current = onClick
  }, [onClick])

  useEffect(() => {
    if (!ref.current) return
    const c = echarts.init(ref.current)
    chart.current = c
    c.on('click', (p) => click.current?.(p as ChartClick))
    const ro = new ResizeObserver(() => c.resize())
    ro.observe(ref.current)
    return () => {
      ro.disconnect()
      c.dispose()
      chart.current = undefined
    }
  }, [])

  useEffect(() => {
    chart.current?.setOption(option, true)
  }, [option])

  return <div ref={ref} className="telemetry-chart" style={{ height }} />
}
