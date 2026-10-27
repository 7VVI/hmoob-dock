import { mount } from '@vue/test-utils'
import { expect, test } from 'vitest'
import MetricChart from './MetricChart.vue'
import { chartSeries } from '../api/metrics'
import type { MetricPoint } from '../api/types'

// point builds one point of a curve.
function point(at: string, average: number | null, peak: number | null, complete = true): MetricPoint {
  return { at, average, peak, complete }
}

test('names the unit of the curve', () => {
  const wrapper = mount(MetricChart, {
    props: { points: [point('2026-10-25T08:00:00Z', 10, 20)], unit: '每核 100%' },
  })
  expect(wrapper.text()).toContain('每核 100%')
})

test('turns an empty curve into no value instead of a zero', () => {
  const wrapper = mount(MetricChart, { props: { points: [], unit: 'MiB' } })
  expect(wrapper.text()).toContain('没有数据')
  expect(wrapper.text()).not.toContain('0')
})

test('does not draw a line over a gap', () => {
  const series = chartSeries([
    point('2026-10-25T08:00:00Z', 10, 20),
    point('2026-10-25T08:01:00Z', null, null),
    point('2026-10-25T08:02:00Z', 30, 40),
  ])

  expect(series).toHaveLength(2)
  expect(series.every((line) => line.connectNulls === false)).toBe(true)
  expect(series[0].data).toEqual([
    ['2026-10-25T08:00:00Z', 10],
    ['2026-10-25T08:01:00Z', null],
    ['2026-10-25T08:02:00Z', 30],
  ])
  expect(series[1].data).toEqual([
    ['2026-10-25T08:00:00Z', 20],
    ['2026-10-25T08:01:00Z', null],
    ['2026-10-25T08:02:00Z', 40],
  ])
})

test('keeps a minute without samples empty', () => {
  const series = chartSeries([point('2026-10-25T08:00:00Z', null, null, false)])
  expect(series[0].data).toEqual([['2026-10-25T08:00:00Z', null]])
})
