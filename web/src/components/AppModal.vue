<script setup lang="ts">
import { nextTick, ref, watch } from 'vue'

const props = withDefaults(
  defineProps<{
    // open reports whether the dialog is on the page.
    open: boolean
    // title is the name of the dialog.
    title: string
    // wide lets a dialog that carries a wide body grow past the default.
    wide?: boolean
  }>(),
  { wide: false },
)

const emit = defineEmits<{ close: [] }>()

// body holds the first field of the dialog, so a keyboard starts where the
// administrator types.
const body = ref<HTMLDivElement | null>(null)

watch(
  () => props.open,
  (open) => {
    if (open) {
      void nextTick(() => {
        const focusable = body.value?.querySelector<HTMLElement>(
          'input, select, textarea, button',
        )
        focusable?.focus()
      })
    }
  },
)

// close asks the caller to close the dialog.
function close(): void {
  emit('close')
}
</script>

<template>
  <Teleport to="body">
    <div v-if="open" class="overlay" @click.self="close" @keydown.escape="close">
      <div
        class="dialog"
        :class="{ wide }"
        role="dialog"
        aria-modal="true"
        :aria-label="title"
      >
        <header class="dialog-head">
          <span class="head-icon" aria-hidden="true">
            <slot name="icon">
              <svg viewBox="0 0 24 24">
                <path d="M12 5v14M5 12h14" />
              </svg>
            </slot>
          </span>
          <h2>{{ title }}</h2>
          <button type="button" class="close" aria-label="关闭" @click="close">
            <svg viewBox="0 0 24 24"><path d="M6 6l12 12M18 6 6 18" /></svg>
          </button>
        </header>
        <div ref="body" class="dialog-body">
          <slot />
        </div>
        <footer v-if="$slots.footer" class="dialog-foot">
          <slot name="footer" />
        </footer>
      </div>
    </div>
  </Teleport>
</template>

<style scoped>
/* The chrome of every dialog of the console: the same overlay, the same
   header and the same face, so a page never grows a second kind of
   dialog. */
.overlay {
  position: fixed;
  inset: 0;
  z-index: 80;
  display: grid;
  place-items: center;
  padding: 24px;
  background: rgb(12 24 48 / 45%);
  backdrop-filter: blur(2px);
}

.dialog {
  width: 100%;
  max-width: 640px;
  max-height: min(88vh, 900px);
  display: flex;
  flex-direction: column;
  border-radius: 16px;
  background: #fff;
  box-shadow: 0 24px 60px -24px rgb(12 24 48 / 45%);
  overflow: hidden;
}

.dialog.wide {
  max-width: 860px;
}

.dialog-head {
  display: flex;
  align-items: center;
  gap: 12px;
  flex: none;
  padding: 18px 22px;
  border-bottom: 1px solid #eef2fa;
  background: linear-gradient(120deg, #f6faff 0%, #fff 55%, #eef5ff 100%);
}

.head-icon {
  width: 38px;
  height: 38px;
  display: grid;
  place-items: center;
  border-radius: 11px;
  background: #e9f2ff;
  color: #146ef5;
  flex: none;
}

.head-icon svg {
  width: 19px;
  height: 19px;
  fill: none;
  stroke: currentcolor;
  stroke-width: 1.9;
  stroke-linecap: round;
}

.dialog-head h2 {
  margin: 0;
  font-size: 17px;
  font-weight: 700;
  color: #132a56;
  letter-spacing: 0.01em;
}

.close {
  appearance: none;
  margin-left: auto;
  width: 32px;
  height: 32px;
  display: grid;
  place-items: center;
  border: none;
  border-radius: 8px;
  background: none;
  color: #667085;
  cursor: pointer;
}

.close:hover {
  background: #f2f4f7;
  color: #132a56;
}

.close svg {
  width: 16px;
  height: 16px;
  fill: none;
  stroke: currentcolor;
  stroke-width: 2;
  stroke-linecap: round;
}

.dialog-body {
  overflow-y: auto;
  padding: 20px 22px;
}

.dialog-foot {
  flex: none;
  display: flex;
  justify-content: flex-end;
  gap: 12px;
  padding: 14px 22px 18px;
  border-top: 1px solid #eef2fa;
}

@media (max-width: 640px) {
  .overlay {
    padding: 12px;
  }
}
</style>
