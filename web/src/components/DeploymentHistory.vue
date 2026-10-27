<script setup lang="ts">
import { computed } from 'vue'
import { formatTimestamp, type VersionSummary } from '../api/types'

const props = defineProps<{
  // versions are the versions of the application, the newest one first.
  versions: VersionSummary[]
  // latestVersionId is the version that was submitted last.
  latestVersionId: string
  // successfulVersionId is the version that was deployed successfully last.
  successfulVersionId: string
  // busy reports that the page is busy with another action.
  busy: boolean
}>()

const emit = defineEmits<{
  (event: 'redeploy', versionID: string): void
}>()

// marks names the roles a version has in the application.
const marks = computed(() => {
  const result = new Map<string, string[]>()
  for (const version of props.versions) {
    const names: string[] = []
    if (version.id === props.latestVersionId) {
      names.push('最近提交')
    }
    if (version.id === props.successfulVersionId) {
      names.push('最近成功')
    }
    result.set(version.id, names)
  }
  return result
})
</script>

<template>
  <div class="history">
    <p class="muted">
      重新部署该版本只会把该版本的配置再次部署到目标上，不会恢复已经删除的数据。
    </p>

    <p v-if="versions.length === 0" class="empty">还没有部署过这个应用。</p>

    <div v-else class="table-scroll">
      <table class="nodes">
        <caption class="visually-hidden">应用的部署记录</caption>
        <thead>
          <tr>
            <th scope="col">时间</th>
            <th scope="col">版本</th>
            <th scope="col">标记</th>
            <th scope="col">
              <span class="visually-hidden">操作</span>
            </th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="version in versions" :key="version.id">
            <td class="nodes__seen">{{ formatTimestamp(version.created_at) }}</td>
            <th scope="row" class="nodes__host"><code>{{ version.id }}</code></th>
            <td class="muted">{{ (marks.get(version.id) ?? []).join('、') || '—' }}</td>
            <td class="nodes__actions">
              <button
                type="button"
                class="button button--quiet"
                :disabled="busy"
                @click="emit('redeploy', version.id)"
              >
                重新部署该版本
              </button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
