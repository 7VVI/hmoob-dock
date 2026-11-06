<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { NButton } from 'naive-ui'

const props = defineProps<{
  // lines are the pieces of the log of the container.
  lines: string[]
  // truncated reports that a piece of the log was lost.
  truncated: boolean
}>()

// maxLineLength is the length of the longest line the viewer shows.
//
// A line that is longer is cut and marked, so the reader knows that the
// line continues.
const maxLineLength = 2000

// displayLines are the lines of the log as they are shown.
const displayLines = computed(() =>
  props.lines.map((line) =>
    line.length > maxLineLength ? `${line.slice(0, maxLineLength)}…（本行过长，已截断）` : line,
  ),
)

// text is the log as one piece of text.
//
// The log is written as text and never as markup, so a container that writes
// markup cannot make the page do anything.
const text = computed(() => displayLines.value.join('\n'))

// The scroller is the element that holds the log.
const scroller = ref<HTMLElement | null>(null)

// following reports whether the log follows the newest line.
//
// A reader that scrolls up to read something stops the log from moving on
// its own. The log of the server keeps running: only the page stops
// following it.
const following = ref(true)

// scrollToBottom shows the newest line of the log.
function scrollToBottom(): void {
  const element = scroller.value
  if (!element) {
    return
  }
  element.scrollTop = element.scrollHeight
}

// onScroll notices that the reader left the newest line.
function onScroll(): void {
  const element = scroller.value
  if (!element) {
    return
  }
  const distance = element.scrollHeight - element.scrollTop - element.clientHeight
  following.value = distance < 8
}

// follow shows the newest line of the log again.
function follow(): void {
  following.value = true
  scrollToBottom()
}

watch(
  () => props.lines.length,
  async () => {
    if (!following.value) {
      return
    }
    await nextTick()
    scrollToBottom()
  },
)
</script>

<template>
  <div class="grid gap-2">
    <p
      v-if="truncated"
      class="m-0 rounded-lg border border-[#d3dcec] bg-[#fffaeb] px-3 py-2 text-[13px] text-[#b54708]"
    >
      部分日志缺失，上面的内容不是连续的。
    </p>
    <p v-if="lines.length === 0" class="py-4 text-[#667085]">还没有日志。</p>
    <template v-else>
      <pre
        ref="scroller"
        class="m-0 max-h-[26rem] overflow-auto rounded-lg border border-[#e6ecf7] bg-[#f2f6fe] p-3 font-mono text-[12.8px] leading-relaxed whitespace-pre-wrap break-all"
        @scroll="onScroll"
      >{{ text }}</pre>
      <NButton v-if="!following" quaternary type="primary" size="small" class="justify-self-start" @click="follow">
        跟随最新
      </NButton>
    </template>
  </div>
</template>
