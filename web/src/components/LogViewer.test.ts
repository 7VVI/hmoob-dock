import { mount } from '@vue/test-utils'
import { expect, test } from 'vitest'
import LogViewer from './LogViewer.vue'

test('renders container output as text', () => {
  const payload = '<img src=x onerror=alert(1)>'
  const wrapper = mount(LogViewer, { props: { lines: [payload], truncated: false } })
  expect(wrapper.text()).toContain(payload)
  expect(wrapper.find('img').exists()).toBe(false)
})

test('never runs the markup of a log line', () => {
  const wrapper = mount(LogViewer, {
    props: { lines: ['<script>alert(1)</script>'], truncated: false },
  })
  expect(wrapper.find('script').exists()).toBe(false)
  expect(wrapper.find('pre').text()).toContain('<script>alert(1)</script>')
})

test('reports the gap of the logs', () => {
  const wrapper = mount(LogViewer, { props: { lines: ['first'], truncated: true } })
  expect(wrapper.text()).toContain('部分日志缺失')
})

test('keeps a quiet log free of a gap notice', () => {
  const wrapper = mount(LogViewer, { props: { lines: ['first'], truncated: false } })
  expect(wrapper.text()).not.toContain('部分日志缺失')
})

test('marks a line that is too long', () => {
  const long = 'x'.repeat(5000)
  const wrapper = mount(LogViewer, { props: { lines: [long], truncated: false } })
  expect(wrapper.text()).toContain('本行过长')
  expect(wrapper.find('pre').text().length).toBeLessThan(long.length)
})

test('shows every line of the log', () => {
  const wrapper = mount(LogViewer, {
    props: { lines: ['first', 'second', 'third'], truncated: false },
  })
  const text = wrapper.find('pre').text()
  expect(text).toContain('first')
  expect(text).toContain('second')
  expect(text).toContain('third')
})
