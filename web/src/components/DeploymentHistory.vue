<script setup lang="ts">
import { computed } from 'vue'
import { NButton, NTag } from 'naive-ui'
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
  <div class="grid gap-4">
    <p class="text-[13px] text-[#667085]">
      重新部署该版本只会把该版本的配置再次部署到目标上，不会恢复已经删除的数据。
    </p>

    <p v-if="versions.length === 0" class="py-6 text-center text-[#667085]">还没有部署过这个应用。</p>

    <div v-else class="overflow-x-auto">
      <table class="w-full border-collapse text-left text-[13px]">
        <caption class="sr-only">应用的部署记录</caption>
        <thead>
          <tr>
            <th scope="col" class="whitespace-nowrap px-5 py-3 text-xs font-medium text-[#667085]">时间</th>
            <th scope="col" class="whitespace-nowrap px-5 py-3 text-xs font-medium text-[#667085]">版本</th>
            <th scope="col" class="whitespace-nowrap px-5 py-3 text-xs font-medium text-[#667085]">标记</th>
            <th scope="col" class="px-5 py-3"><span class="sr-only">操作</span></th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="version in versions"
            :key="version.id"
            class="border-b border-[#e6ecf7] last:border-b-0 hover:bg-[#f7faff]"
          >
            <td class="whitespace-nowrap px-5 py-4 font-mono text-xs tabular-nums text-[#344054]">
              {{ formatTimestamp(version.created_at) }}
            </td>
            <th scope="row" class="whitespace-nowrap px-5 py-4 text-[13px] font-medium">
              <code>{{ version.id }}</code>
            </th>
            <td class="whitespace-nowrap px-5 py-4">
              <NTag
                v-for="name in marks.get(version.id) ?? []"
                :key="name"
                size="small"
                round
                :bordered="false"
                type="info"
                class="mr-1"
              >
                {{ name }}
              </NTag>
              <span v-if="(marks.get(version.id) ?? []).length === 0" class="text-[#667085]">—</span>
            </td>
            <td class="whitespace-nowrap px-5 py-4 text-right">
              <NButton quaternary type="primary" size="small" :disabled="busy" @click="emit('redeploy', version.id)">
                重新部署该版本
              </NButton>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
