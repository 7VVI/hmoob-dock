<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import type { ECharts } from 'echarts/core'
import { chartSeries } from '../api/metrics'
import type { MetricPoint } from '../api/types'

const props = defineProps<{
  // points are the minutes of the window. A minute that was not observed
  // holds no value, so the curve shows a gap there.
  points: MetricPoint[]
  // unit names the unit of the values of the curve.
  unit: string
  // title names the curve for the reader.
  title?: string
  // format writes a value in the unit of the curve.
  format?: (value: number) => string
}>()

const canvas = ref<HTMLElement | null>(null)

let chart: ECharts | null = null
let observer: ResizeObserver | null = null
// disposed reports that the page left the curve behind.
let disposed = false

const hasData = computed(() => props.points.length > 0)

// description names the curve for a screen reader.
const description = computed(() => `${props.title ?? '容器'}资源曲线，单位 ${props.unit}`)

// scale writes a value in the unit of the curve.
function scale(value: number): string {
  return props.format ? props.format(value) : String(value)
}

// timeLabel writes a moment of the curve for the reader.
function timeLabel(at: string): string {
  const moment = new Date(at)
  if (Number.isNaN(moment.getTime())) {
    return at
  }
  return moment.toLocaleString('zh-CN', { hour12: false })
}

// tooltip names the values of one minute.
//
// A minute whose samples are not complete says so: the reader knows that the
// value is a mean of fewer samples than a full minute holds.
function tooltip(params: unknown): string {
  const entries = (Array.isArray(params) ? params : [params]) as {
    seriesName?: string
    value?: unknown
    axisValue?: string
  }[]
  let at = ''
  const lines: string[] = []
  for (const entry of entries) {
    const value = Array.isArray(entry.value) ? entry.value[1] : null
    if (typeof value !== 'number') {
      continue
    }
    if (at === '' && typeof entry.axisValue === 'string') {
      at = entry.axisValue
    }
    lines.push(`${entry.seriesName ?? ''}：${scale(value)} ${props.unit}`)
  }
  const point = props.points.find((item) => item.at === at)
  const note = point && !point.complete ? '这一分钟的样本不完整' : ''
  const head = at === '' ? '' : `${timeLabel(at)}`
  return [head, note, ...lines].filter((part) => part !== '').join('<br/>')
}

// option builds the option of the curve.
//
// The two lines never join over a gap, so a minute that was not observed
// stays visible as a gap instead of a line that never existed.
function option() {
  // The curve is drawn on a canvas, so the colours are the literal values of
  // the palette: a canvas does not resolve a custom property.
  const accent = '#0255f0'
  const ink = '#101828'
  const muted = '#667085'
  const line = '#e6ecf7'
  const grid = '#edf2fb'
  const peak = '#8fa6cf'
  const mono = 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace'
  return {
    animation: false,
    grid: { left: 56, right: 20, top: 16, bottom: 40 },
    legend: {
      data: ['平均值', '峰值'],
      bottom: 0,
      itemWidth: 16,
      itemHeight: 8,
      textStyle: { color: muted, fontSize: 11, fontFamily: mono },
    },
    tooltip: {
      trigger: 'axis',
      formatter: tooltip,
      backgroundColor: '#ffffff',
      borderColor: line,
      borderWidth: 1,
      padding: [8, 10],
      textStyle: { color: ink, fontSize: 12, fontFamily: mono },
    },
    xAxis: {
      type: 'time',
      axisLine: { lineStyle: { color: line } },
      axisTick: { show: false },
      axisLabel: { color: muted, fontSize: 11, fontFamily: mono },
      splitLine: { show: false },
    },
    yAxis: {
      type: 'value',
      // The readings of an idle container sit close to zero, so the axis
      // follows the data instead of starting at a full range.
      scale: true,
      name: props.unit,
      nameTextStyle: { color: muted, fontSize: 11, align: 'left' },
      axisLine: { show: false },
      axisTick: { show: false },
      axisLabel: { color: muted, fontSize: 11, fontFamily: mono },
      splitLine: { lineStyle: { color: grid, width: 1 } },
    },
    series: chartSeries(props.points).map((entry, index) => ({
      ...entry,
      showSymbol: false,
      smooth: false,
      lineStyle: {
        width: index === 0 ? 2 : 1.5,
        type: index === 0 ? 'solid' : 'dashed',
        color: index === 0 ? accent : peak,
      },
      itemStyle: { color: index === 0 ? accent : peak },
    })),
  }
}

// render draws the curve.
function render(): void {
  if (!chart) {
    return
  }
  chart.setOption(option(), true)
}

// loadRuntime brings the library of the curve.
//
// The library is loaded when the page shows a curve for the first time, so a
// page that shows no curve does not carry it. Only the parts of the library
// that the curve needs are loaded, so the whole of it never travels to the
// browser.
async function loadRuntime(): Promise<typeof import('./chartRuntime')> {
  return import('./chartRuntime')
}

// setup draws the curve as soon as the page gives it a size.
//
// A container without a size cannot be drawn, so the curve waits for the
// page to show it.
async function setup(): Promise<void> {
  const element = canvas.value
  if (!element || chart || disposed) {
    return
  }
  if (element.clientWidth === 0 || element.clientHeight === 0) {
    waitForSize()
    return
  }
  try {
    const runtime = await loadRuntime()
    // The page may have left the curve behind while the library was on its
    // way, so the curve is only drawn when it is still on the page.
    if (disposed || canvas.value !== element) {
      return
    }
    chart = runtime.createChart(element)
  } catch {
    // A browser that cannot draw the curve shows the values as text
    // instead of an empty box.
    chart = null
    return
  }
  render()
}

// waitForSize watches the container until the page gives it a size.
function waitForSize(): void {
  if (observer || typeof ResizeObserver === 'undefined') {
    return
  }
  const element = canvas.value
  if (!element) {
    return
  }
  observer = new ResizeObserver(() => {
    if (element.clientWidth > 0 && element.clientHeight > 0) {
      observer?.disconnect()
      observer = null
      void setup()
    }
  })
  observer.observe(element)
}

// resize keeps the curve in step with the page.
function resize(): void {
  chart?.resize()
}

watch(
  () => props.points,
  () => {
    void setup()
    render()
  },
  { flush: 'post' },
)

onMounted(() => {
  void setup()
  window.addEventListener('resize', resize)
})

onBeforeUnmount(() => {
  disposed = true
  window.removeEventListener('resize', resize)
  observer?.disconnect()
  observer = null
  chart?.dispose()
  chart = null
})
</script>

<template>
  <figure class="chart">
    <figcaption class="chart__caption">
      <span v-if="title" class="chart__title">{{ title }}</span>
      <span class="chart__unit">{{ unit }}</span>
    </figcaption>
    <p v-if="!hasData" class="empty">这个时间范围内没有数据。</p>
    <div v-else ref="canvas" class="chart__canvas" role="img" :aria-label="description" />
  </figure>
</template>

<style scoped>
.chart {
  margin: 0;
}

.chart__caption {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: var(--space-2) var(--space-3);
  margin-bottom: var(--space-2);
  font-size: 0.9rem;
}

.chart__title {
  font-weight: 600;
}

.chart__unit {
  color: var(--color-text-muted);
}

.chart__canvas {
  width: 100%;
  height: 16rem;
}
</style>
