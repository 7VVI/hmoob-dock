import { mount } from '@vue/test-utils'
import { expect, test } from 'vitest'
import AlertRuleModal from './AlertRuleModal.vue'
import type { AlertRule } from '../api/types'

// A rule of the resource of a container.
const RESOURCE_RULE: AlertRule = {
  id: 'rule-1',
  name: '容器CPU使用率告警',
  description: '',
  kind: 'resource',
  severity: 'warning',
  scope_kind: 'container',
  scope_id: 'container-1',
  metric: 'cpu_percent',
  operator: '>',
  threshold: 80,
  for_seconds: 60,
  enabled: true,
  recover_notify: true,
}

// mountModal mounts the dialog with the teleport rendered in place, so the
// fields of the dialog are reachable through the wrapper.
function mountModal(props: Record<string, unknown> = {}) {
  return mount(AlertRuleModal, {
    props: { open: true, ...props },
    global: { stubs: { teleport: true } },
  })
}

// valueOf reads the value of a field of the dialog.
function valueOf(wrapper: ReturnType<typeof mount>, selector: string): string {
  return (wrapper.find(selector).element as HTMLInputElement).value
}

test('asks for the name, the description and the condition of a resource rule', () => {
  const wrapper = mountModal({ rule: RESOURCE_RULE })

  expect(wrapper.find('[name="name"]').exists()).toBe(true)
  expect(wrapper.find('[name="description"]').exists()).toBe(true)
  expect(wrapper.find('[name="severity"]').exists()).toBe(true)
  expect(wrapper.find('[name="scope_kind"]').exists()).toBe(true)
  expect(wrapper.find('[name="metric"]').exists()).toBe(true)
  expect(wrapper.find('[name="operator"]').exists()).toBe(true)
  expect(wrapper.find('[name="threshold"]').exists()).toBe(true)
  expect(wrapper.find('[name="for_seconds"]').exists()).toBe(true)
  expect(valueOf(wrapper, '[name="name"]')).toBe('容器CPU使用率告警')
  expect(valueOf(wrapper, '[name="severity"]:checked')).toBe('warning')
  expect(valueOf(wrapper, '[name="threshold"]')).toBe('80')
  expect(valueOf(wrapper, '[name="for_seconds"]')).toBe('60')
})

test('titles the dialog after the change it makes', () => {
  const created = mountModal()
  expect(created.text()).toContain('创建告警规则')

  const edited = mountModal({ rule: RESOURCE_RULE, editing: true })
  expect(edited.text()).toContain('编辑告警规则')
})

test('sends the values of the dialog to the caller', async () => {
  const wrapper = mountModal({ rule: RESOURCE_RULE })

  await wrapper.find('[name="description"]').setValue('CPU 过载时提醒')
  await wrapper.find('[name="scope_id"]').setValue('container-2')
  await wrapper.find('[name="threshold"]').setValue('90')
  await wrapper.find('[name="for_seconds"]').setValue('120')
  await wrapper.find('form').trigger('submit')

  const saved = wrapper.emitted('save')
  expect(saved).toHaveLength(1)
  expect(saved![0][0]).toMatchObject({
    name: '容器CPU使用率告警',
    description: 'CPU 过载时提醒',
    kind: 'resource',
    severity: 'warning',
    scope_kind: 'container',
    scope_id: 'container-2',
    metric: 'cpu_percent',
    operator: '>',
    threshold: 90,
    for_seconds: 120,
    enabled: true,
    recover_notify: true,
  })
})

test('sends the name and the severity the administrator chose', async () => {
  const wrapper = mountModal()

  await wrapper.find('[name="name"]').setValue('容器内存告警')
  await wrapper.find('[name="severity"][value="critical"]').setValue(true)
  await wrapper.find('[name="scope_id"]').setValue('container-3')
  await wrapper.find('[name="threshold"]').setValue('85')
  await wrapper.find('form').trigger('submit')

  const saved = wrapper.emitted('save')
  expect(saved).toHaveLength(1)
  expect(saved![0][0]).toMatchObject({ name: '容器内存告警', severity: 'critical' })
})

test('refuses a rule without a name', async () => {
  const wrapper = mountModal()

  await wrapper.find('[name="name"]').setValue('   ')
  await wrapper.find('form').trigger('submit')

  expect(wrapper.emitted('save')).toBeUndefined()
  expect(wrapper.text()).toContain('请填写这条规则的名称。')
})

test('hides the object of a rule that watches every container', async () => {
  const wrapper = mountModal({ rule: null })

  await wrapper.find('[name="scope_kind"][value="all"]').setValue(true)

  expect(wrapper.find('[name="scope_id"]').exists()).toBe(false)
})

test('does not ask for a limit of a rule that watches an end', async () => {
  const wrapper = mountModal({ rule: null })

  await wrapper.find('[name="kind"][value="container_exit"]').setValue(true)

  expect(wrapper.find('[name="metric"]').exists()).toBe(false)
  expect(wrapper.find('[name="operator"]').exists()).toBe(false)
  expect(wrapper.find('[name="threshold"]').exists()).toBe(false)
  expect(wrapper.find('[name="for_seconds"]').exists()).toBe(true)
})

test('asks for a node of a rule that watches a lost agent', async () => {
  const wrapper = mountModal({ rule: null })

  await wrapper.find('[name="kind"][value="agent_offline"]').setValue(true)

  expect(wrapper.find('[name="scope_kind"]').exists()).toBe(false)
  expect(wrapper.find('[name="scope_id"]').exists()).toBe(true)
  expect(wrapper.find('[name="for_seconds"]').exists()).toBe(true)
})

test('keeps the pause of a rule the dialog edits', async () => {
  const wrapper = mountModal({ rule: { ...RESOURCE_RULE, enabled: false }, editing: true })

  await wrapper.find('form').trigger('submit')

  const saved = wrapper.emitted('save')
  expect(saved).toHaveLength(1)
  expect(saved![0][0]).toMatchObject({ enabled: false })
})

test('shows the reason of a refused rule and keeps the input', async () => {
  const wrapper = mountModal({ rule: RESOURCE_RULE })

  await wrapper.find('[name="threshold"]').setValue('95')
  await wrapper.setProps({ errors: { threshold: '资源告警必须指定一个大于零的阈值' } })

  expect(wrapper.text()).toContain('资源告警必须指定一个大于零的阈值')
  expect(valueOf(wrapper, '[name="threshold"]')).toBe('95')
})

test('shows the reason the control端 refused the rule as a whole', () => {
  const wrapper = mountModal({ error: '这条告警规则的内容不合法' })
  expect(wrapper.text()).toContain('这条告警规则的内容不合法')
})

test('asks for the type of a rule whose type is unknown', () => {
  const wrapper = mountModal({
    rule: { ...RESOURCE_RULE, kind: 'unknown' as AlertRule['kind'] },
  })
  expect((wrapper.find('[name="kind"][value="resource"]').element as HTMLInputElement).checked).toBe(false)
  expect(wrapper.text()).not.toContain('当容器')
})

test('falls back to a warning for a rule of an unknown severity', () => {
  const wrapper = mountModal({
    rule: { ...RESOURCE_RULE, severity: 'urgent' as AlertRule['severity'] },
  })
  expect((wrapper.find('[name="severity"][value="warning"]').element as HTMLInputElement).checked).toBe(true)
})

test('previews the condition while the administrator types', async () => {
  const wrapper = mountModal({ rule: RESOURCE_RULE })
  expect(wrapper.text()).toContain('当容器 CPU 连续 60 秒 > 80% 时触发告警')

  await wrapper.find('[name="operator"]').setValue('>=')
  await wrapper.find('[name="threshold"]').setValue('90')
  await wrapper.find('[name="for_seconds"]').setValue('30')
  expect(wrapper.text()).toContain('当容器 CPU 连续 30 秒 ≥ 90% 时触发告警')
})

test('toggles the recovery of the rule', async () => {
  const wrapper = mountModal({ rule: RESOURCE_RULE })

  expect((wrapper.find('[role="switch"]').element as HTMLButtonElement).getAttribute('aria-checked')).toBe('true')
  await wrapper.find('[role="switch"]').trigger('click')
  expect((wrapper.find('[role="switch"]').element as HTMLButtonElement).getAttribute('aria-checked')).toBe('false')

  await wrapper.find('form').trigger('submit')
  const saved = wrapper.emitted('save')
  expect(saved).toHaveLength(1)
  expect(saved![0][0]).toMatchObject({ recover_notify: false })
})

test('closes through the cancel and the close of the dialog', async () => {
  const wrapper = mountModal()

  await wrapper.find('.ghost-button').trigger('click')
  expect(wrapper.emitted('cancel')).toHaveLength(1)

  await wrapper.find('.close').trigger('click')
  expect(wrapper.emitted('cancel')).toHaveLength(2)
})

test('renders nothing while the dialog is closed', () => {
  const wrapper = mountModal({ open: false })
  expect(wrapper.find('.dialog').exists()).toBe(false)
})
