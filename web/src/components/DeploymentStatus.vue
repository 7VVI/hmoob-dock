<script setup lang="ts">
import { computed } from 'vue'
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
function toneOf(state: TaskState): string {
  switch (state) {
    case 'succeeded':
      return 'state--online'
    case 'failed':
      return 'state--danger'
    case 'uncertain':
      return 'state--unknown'
    default:
      return 'state--pending'
  }
}

const label = computed(() => labelOf(props.state))
const tone = computed(() => toneOf(props.state))
</script>

<template>
  <p class="deployment-state">
    <span class="state" :class="tone">{{ label }}</span>
    <span v-if="state === 'uncertain'" class="muted">
      结果未知，需要人工核对后才能继续。
    </span>
    <span v-if="partialUpdate" class="muted">部分更新：只有一部分服务到达了目标状态。</span>
    <span v-if="rolledBack" class="muted">目标已回滚到本次部署之前的状态。</span>
  </p>
</template>

<style scoped>
.deployment-state {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: var(--space-2) var(--space-4);
}

.state--pending {
  color: var(--color-accent);
}

.state--danger {
  color: var(--color-danger);
}

.state--unknown {
  color: var(--color-offline);
}
</style>
