import { mount } from '@vue/test-utils'
import { expect, test } from 'vitest'
import AlertRuleForm from './AlertRuleForm.vue'
import type { AlertRule } from '../api/types'

// A rule of the resource of a container.
const RESOURCE_RULE: AlertRule = {
  id: 'rule-1',
  kind: 'resource',
  scope_id: 'container-1',
  metric: 'cpu_percent',
  threshold: 80,
  for_seconds: 60,
  enabled: true,
}

// valueOf reads the value of a field of the form.
function valueOf(wrapper: ReturnType<typeof mount>, selector: string): string {
  return (wrapper.find(selector).element as HTMLInputElement).value
}

test('asks for the threshold and the duration of a resource rule', () => {
  const wrapper = mount(AlertRuleForm, { props: { rule: RESOURCE_RULE } })

  expect(wrapper.find('[name="threshold"]').exists()).toBe(true)
  expect(wrapper.find('[name="for_seconds"]').exists()).toBe(true)
  expect(valueOf(wrapper, '[name="threshold"]')).toBe('80')
  expect(valueOf(wrapper, '[name="for_seconds"]')).toBe('60')
})

test('sends the values of the form to the caller', async () => {
  const wrapper = mount(AlertRuleForm, { props: { rule: RESOURCE_RULE } })

  await wrapper.find('[name="scope_id"]').setValue('container-2')
  await wrapper.find('[name="threshold"]').setValue('90')
  await wrapper.find('[name="for_seconds"]').setValue('120')
  await wrapper.find('form').trigger('submit')

  const saved = wrapper.emitted('save')
  expect(saved).toHaveLength(1)
  expect(saved![0][0]).toMatchObject({
    kind: 'resource',
    scope_id: 'container-2',
    metric: 'cpu_percent',
    threshold: 90,
    for_seconds: 120,
    enabled: true,
  })
})

test('does not ask for a limit of a rule that watches an end', async () => {
  const wrapper = mount(AlertRuleForm, { props: { rule: null } })

  await wrapper.find('[name="kind"]').setValue('container_exit')

  expect(wrapper.find('[name="metric"]').exists()).toBe(false)
  expect(wrapper.find('[name="threshold"]').exists()).toBe(false)
  expect(wrapper.find('[name="for_seconds"]').exists()).toBe(true)
})

test('keeps the input and shows the reason of a refused rule', async () => {
  const wrapper = mount(AlertRuleForm, { props: { rule: RESOURCE_RULE } })

  await wrapper.find('[name="threshold"]').setValue('95')
  await wrapper.setProps({ errors: { threshold: '资源告警必须指定一个大于零的阈值' } })

  expect(wrapper.text()).toContain('资源告警必须指定一个大于零的阈值')
  expect(valueOf(wrapper, '[name="threshold"]')).toBe('95')
})

test('asks for the type of a rule whose type is unknown', () => {
  const wrapper = mount(AlertRuleForm, {
    props: { rule: { ...RESOURCE_RULE, kind: 'unknown' as AlertRule['kind'] } },
  })
  expect(valueOf(wrapper, '[name="kind"]')).toBe('')
  expect(wrapper.text()).toContain('请选择类型')
})
