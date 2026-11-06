<script setup lang="ts">
import { computed } from 'vue'
import { NTag } from 'naive-ui'
import type { TaskState } from '../api/types'

const props = defineProps<{
  // state is the phase the deployment is in.
  state: TaskState
  // partialUpdate reports that only a part of the application changed.
  partialUpdate: boolean
  // rolledBack reports that the target undid the change.
  rolledBack: boolean
}>()

// labelOf names the phase of a deployment.
//
// A deployment whose outcome could not be proven is not a failure: it waits
// for the check of the administrator, so it gets a name of its own.
function labelOf(state: TaskState): string {
  switch (state) {
    case 'queued':
      return '排队中'
    case 'preflight':
      return '预检中'
    case 'running':
      return '部署中'
    case 'verifying':
      return '验证中'
    case 'succeeded':
      return '部署成功'
    case 'failed':
      return '部署失败'
    case 'uncertain':
      return '状态待确认'
  }
}

// toneOf names the kind of the phase.
//
// The phase whose outcome is unknown has a tone of its own, so the page
// never paints it like a failure.
function toneOf(state: TaskState): 'success' | 'error' | 'warning' | 'info' {
  switch (state) {
    case 'succeeded':
      return 'success'
    case 'failed':
      return 'error'
    case 'uncertain':
      return 'warning'
    default:
      return 'info'
  }
}

const label = computed(() => labelOf(props.state))
const tone = computed(() => toneOf(props.state))
</script>

<template>
  <p class="flex flex-wrap items-baseline gap-x-6 gap-y-2">
    <NTag size="small" round :bordered="false" :type="tone">{{ label }}</NTag>
    <span v-if="state === 'uncertain'" class="text-[#667085]">
      结果未知，需要人工核对后才能继续。
    </span>
    <span v-if="partialUpdate" class="text-[#667085]">部分更新：只有一部分服务到达了目标状态。</span>
    <span v-if="rolledBack" class="text-[#667085]">目标已回滚到本次部署之前的状态。</span>
  </p>
</template>
