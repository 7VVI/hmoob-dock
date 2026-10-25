import { mount } from '@vue/test-utils'
import { expect, test } from 'vitest'
import NodeTable from './NodeTable.vue'

test('reports a disconnected agent without claiming the host is down', () => {
  const wrapper = mount(NodeTable, { props: { nodes: [{
    id: 'node-1', hostname: 'worker-1', connected: false,
    container_count: 3, last_seen: null,
  }] } })
  expect(wrapper.text()).toContain('连接中断')
  expect(wrapper.text()).not.toContain('服务器宕机')
})

test('shows a connected agent with its number of containers', () => {
  const wrapper = mount(NodeTable, { props: { nodes: [{
    id: 'node-1', hostname: 'worker-1', connected: true,
    container_count: 3, last_seen: '2026-10-25T08:00:00Z',
  }] } })
  expect(wrapper.text()).toContain('已连接')
  expect(wrapper.text()).toContain('worker-1')
  expect(wrapper.text()).toContain('3')
  expect(wrapper.text()).not.toContain('连接中断')
})

test('shows an empty state when no node is enrolled', () => {
  const wrapper = mount(NodeTable, { props: { nodes: [] } })
  expect(wrapper.text()).toContain('还没有接入任何节点')
  expect(wrapper.find('table').exists()).toBe(false)
})

test('keeps a long hostname readable', () => {
  const hostname = 'worker-01-with-a-very-long-name-that-does-not-fit-into-one-line.example.internal'
  const wrapper = mount(NodeTable, { props: { nodes: [{
    id: 'node-1', hostname, connected: false,
    container_count: 0, last_seen: null,
  }] } })
  expect(wrapper.text()).toContain(hostname)
})

test('offers an action for every node when the caller brings one', () => {
  const wrapper = mount(NodeTable, {
    props: { nodes: [{
      id: 'node-1', hostname: 'worker-1', connected: false,
      container_count: 0, last_seen: null,
    }] },
    slots: { actions: '<button type="button">撤销</button>' },
  })
  expect(wrapper.find('button').text()).toBe('撤销')
})
