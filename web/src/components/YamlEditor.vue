<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'
import { Compartment, EditorState } from '@codemirror/state'
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

// readonlySlot holds the facets of the read-only state, so the page can
// flip the editor between a document and an editor at runtime.
const readonlySlot = new Compartment()

// readonlyExtensions are the facets the read-only state is made of.
function readonlyExtensions(readOnly: boolean) {
  return [EditorState.readOnly.of(readOnly), EditorView.editable.of(!readOnly)]
}

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
        readonlySlot.of(readonlyExtensions(props.readOnly)),
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

// watch follows the read-only state when it flips at runtime: the page of
// a site shows its configuration as a document and edits it on demand.
watch(
  () => props.readOnly,
  (readOnly) => {
    editor.value?.dispatch({
      effects: readonlySlot.reconfigure(readonlyExtensions(readOnly)),
    })
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
/* The frame of the editor: one rounded box that encloses the whole
   document, the focus shown on the frame and not on an inner outline. */
.yaml-editor {
  border: 1px solid #e3e9f3;
  border-radius: 12px;
  overflow: hidden;
  background: #fafbfc;
  transition: border-color 120ms ease, box-shadow 120ms ease;
}

.yaml-editor:focus-within {
  border-color: #2f6ef0;
  box-shadow: 0 0 0 3px rgb(47 110 240 / 12%);
}

.yaml-editor__surface :deep(.cm-editor) {
  max-height: 26rem;
  font-family: var(--font-mono);
  font-size: 0.85rem;
  background: transparent;
}

.yaml-editor__surface :deep(.cm-editor.cm-focused) {
  outline: none;
}

.yaml-editor__surface :deep(.cm-gutters) {
  background: transparent;
  border-right: 1px solid #edf1f7;
  color: #9aa6bd;
}

.yaml-editor__surface :deep(.cm-scroller) {
  overflow: auto;
}
</style>
