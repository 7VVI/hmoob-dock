<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'
import { EditorState } from '@codemirror/state'
import { EditorView } from '@codemirror/view'
import { yaml } from '@codemirror/lang-yaml'
import { basicSetup } from 'codemirror'

const props = withDefaults(
  defineProps<{
    // modelValue is the document of the editor.
    modelValue: string
    // readOnly keeps the administrator from changing the document.
    readOnly?: boolean
  }>(),
  { readOnly: false },
)

const emit = defineEmits<{
  (event: 'update:modelValue', value: string): void
}>()

// host is the element the editor lives in.
const host = ref<HTMLElement | null>(null)

// editor is the view of CodeMirror.
//
// The view is exposed, so the page can focus the editor and the tests can
// read the state of the document.
const editor = shallowRef<EditorView | null>(null)

onMounted(() => {
  if (!host.value) {
    return
  }
  editor.value = new EditorView({
    parent: host.value,
    state: EditorState.create({
      doc: props.modelValue,
      extensions: [
        // The document is YAML, so the parser of YAML marks it. The page
        // never marks the syntax by hand.
        basicSetup,
        yaml(),
        EditorView.lineWrapping,
        EditorState.readOnly.of(props.readOnly),
        EditorView.editable.of(!props.readOnly),
        EditorView.updateListener.of((update) => {
          if (!update.docChanged) {
            return
          }
          emit('update:modelValue', update.state.doc.toString())
        }),
      ],
    }),
  })
})

onBeforeUnmount(() => {
  editor.value?.destroy()
  editor.value = null
})

// watch replaces the document when the server sends another one.
//
// The document of the editor is only changed when it really differs, so the
// cursor of the administrator stays where it is while he types.
watch(
  () => props.modelValue,
  (value) => {
    const view = editor.value
    if (!view) {
      return
    }
    const current = view.state.doc.toString()
    if (current === value) {
      return
    }
    view.dispatch({ changes: { from: 0, to: current.length, insert: value } })
  },
)

defineExpose({
  get editor(): EditorView | null {
    return editor.value
  },
})
</script>

<template>
  <div class="yaml-editor">
    <div ref="host" class="yaml-editor__surface" data-testid="yaml-editor"></div>
  </div>
</template>

<style scoped>
.yaml-editor {
  border: 1px solid var(--color-border);
  border-radius: var(--radius);
  overflow: hidden;
  background: var(--color-surface);
}

.yaml-editor__surface :deep(.cm-editor) {
  max-height: 26rem;
  font-family: var(--font-mono);
  font-size: 0.85rem;
}

.yaml-editor__surface :deep(.cm-editor.cm-focused) {
  outline: 2px solid var(--color-accent);
  outline-offset: -2px;
}

.yaml-editor__surface :deep(.cm-scroller) {
  overflow: auto;
}
</style>
