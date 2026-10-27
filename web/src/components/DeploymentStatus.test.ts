import { mount } from '@vue/test-utils'
import { expect, test } from 'vitest'
import DeploymentStatus from './DeploymentStatus.vue'

test('keeps unknown deployment distinct from success', () => {
  const wrapper = mount(DeploymentStatus, { props: {
    state: 'uncertain', partialUpdate: false, rolledBack: false,
  } })
  expect(wrapper.text()).toContain('状态待确认')
  expect(wrapper.text()).not.toContain('部署成功')
})

test('keeps unknown deployment distinct from failure', () => {
  const wrapper = mount(DeploymentStatus, { props: {
    state: 'uncertain', partialUpdate: false, rolledBack: false,
  } })
  expect(wrapper.text()).not.toContain('部署失败')
})

test('names the outcome of a finished deployment', () => {
  const succeeded = mount(DeploymentStatus, { props: {
    state: 'succeeded', partialUpdate: false, rolledBack: false,
  } })
  expect(succeeded.text()).toContain('部署成功')

  const failed = mount(DeploymentStatus, { props: {
    state: 'failed', partialUpdate: false, rolledBack: false,
  } })
  expect(failed.text()).toContain('部署失败')
})

test('names the phase of a deployment that is not finished', () => {
  for (const state of ['queued', 'preflight', 'running', 'verifying'] as const) {
    const wrapper = mount(DeploymentStatus, { props: {
      state, partialUpdate: false, rolledBack: false,
    } })
    expect(wrapper.text()).not.toContain('部署失败')
    expect(wrapper.text()).not.toContain('部署成功')
    expect(wrapper.text()).not.toContain('状态待确认')
  }
})

test('reports a partial update', () => {
  const wrapper = mount(DeploymentStatus, { props: {
    state: 'failed', partialUpdate: true, rolledBack: false,
  } })
  expect(wrapper.text()).toContain('部分更新')
})

test('reports a rollback of the target', () => {
  const wrapper = mount(DeploymentStatus, { props: {
    state: 'failed', partialUpdate: false, rolledBack: true,
  } })
  expect(wrapper.text()).toContain('回滚')
})
