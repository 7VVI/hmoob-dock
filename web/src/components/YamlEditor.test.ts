import { mount } from '@vue/test-utils'
import { expect, test } from 'vitest'
import { language } from '@codemirror/language'
import { yamlLanguage } from '@codemirror/lang-yaml'
import { EditorView } from '@codemirror/view'
import YamlEditor from './YamlEditor.vue'

// jsdom does not implement the rectangles of a range of text, which
// CodeMirror asks for when it draws the selection. The stub keeps the
// output of the test clean; the browser answers the question itself.
if (!Range.prototype.getClientRects) {
  Range.prototype.getClientRects = () => [] as unknown as DOMRectList
}

const CONFIG = 'services:\n  web:\n    image: nginx:1.27\n'

test('shows the configuration of the server in the editor', () => {
  const wrapper = mount(YamlEditor, { props: { modelValue: CONFIG } })
  expect(wrapper.find('.cm-content').text()).toContain('services:')
  expect(wrapper.find('.cm-content').text()).toContain('nginx:1.27')
})

test('parses the document as YAML instead of marking it by hand', () => {
  const wrapper = mount(YamlEditor, { props: { modelValue: CONFIG } })
  const editor = wrapper.vm.editor
  expect(editor).not.toBeNull()
  expect(editor!.state.facet(language)).toBe(yamlLanguage)
})

test('reports an edit of the administrator to the caller', async () => {
  const wrapper = mount(YamlEditor, { props: { modelValue: CONFIG } })
  const editor = wrapper.vm.editor!

  editor.dispatch({ changes: { from: 0, to: editor.state.doc.length, insert: 'services: {}\n' } })
  await wrapper.vm.$nextTick()

  expect(wrapper.emitted('update:modelValue')?.at(-1)).toEqual(['services: {}\n'])
})

test('replaces the document when the server sends another configuration', async () => {
  const wrapper = mount(YamlEditor, { props: { modelValue: CONFIG } })
  await wrapper.setProps({ modelValue: 'services:\n  api:\n    image: redis:7\n' })

  const editor = wrapper.vm.editor!
  expect(editor.state.doc.toString()).toBe('services:\n  api:\n    image: redis:7\n')
})

test('does not let the administrator change a document that is read only', () => {
  const wrapper = mount(YamlEditor, { props: { modelValue: CONFIG, readOnly: true } })
  const editor = wrapper.vm.editor!

  expect(editor.state.readOnly).toBe(true)
  expect(editor.state.facet(EditorView.editable)).toBe(false)
})
