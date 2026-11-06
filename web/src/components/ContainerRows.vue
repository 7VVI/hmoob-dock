<script setup lang="ts">
import { NButton, NTag } from 'naive-ui'
import LogViewer from './LogViewer.vue'
import type { ContainerSummary } from '../api/metrics'

// The rows of one group of containers. The page that owns the group hands
// the rows its state and receives every action, so the rows themselves hold
// no state: an answer that arrives late can never land in the wrong place.
defineProps<{
  items: ContainerSummary[]
  operating: string
  confirmingRemove: string
  connected: boolean
  logContainer: string
  logLines: string[]
  logTruncated: boolean
  logError: string
  logBusy: boolean
}>()

const emit = defineEmits<{
  operate: [item: ContainerSummary, action: 'start' | 'stop' | 'restart' | 'remove']
  askRemove: [item: ContainerSummary]
  cancelRemove: []
  watch: [item: ContainerSummary]
}>()

// healthLabel names the state of the health check the way the page of an
// application names it.
function healthLabel(item: ContainerSummary): string {
  if (item.state !== 'running') {
    return '未运行'
  }
  switch (item.health) {
    case 'healthy':
      return '健康'
    case 'unhealthy':
      return '不健康'
    case 'starting':
      return '检查中'
    default:
      return '未配置'
  }
}

function stateType(item: ContainerSummary): 'success' | 'error' {
  return item.state === 'running' ? 'success' : 'error'
}
</script>

<template>
  <div class="overflow-x-auto">
    <table class="w-full border-collapse text-left text-[13px]">
      <caption class="sr-only">容器清单</caption>
      <thead>
        <tr>
          <th scope="col" class="whitespace-nowrap px-4 py-3 text-xs font-medium text-[#667085]">名称</th>
          <th scope="col" class="whitespace-nowrap px-4 py-3 text-xs font-medium text-[#667085]">镜像</th>
          <th scope="col" class="whitespace-nowrap px-4 py-3 text-xs font-medium text-[#667085]">状态</th>
          <th scope="col" class="whitespace-nowrap px-4 py-3 text-xs font-medium text-[#667085]">健康</th>
          <th scope="col" class="whitespace-nowrap px-4 py-3 text-right text-xs font-medium text-[#667085]">操作</th>
        </tr>
      </thead>
      <tbody>
        <tr
          v-for="item in items"
          :key="item.id"
          class="border-b border-[#e6ecf7] last:border-b-0 hover:bg-[#f7faff]"
        >
          <th scope="row" class="whitespace-nowrap px-4 py-3 font-mono text-xs font-medium">
            {{ item.name }}
          </th>
          <td class="px-4 py-3"><code class="break-all text-xs text-[#344054]">{{ item.image }}</code></td>
          <td class="whitespace-nowrap px-4 py-3">
            <NTag size="small" round :bordered="false" :type="stateType(item)">
              {{ item.state === 'running' ? '运行中' : '已停止' }}
            </NTag>
          </td>
          <td class="whitespace-nowrap px-4 py-3">{{ healthLabel(item) }}</td>
          <td class="whitespace-nowrap px-4 py-3">
            <div class="flex flex-wrap items-center justify-end gap-2">
              <NButton quaternary type="primary" size="small" :disabled="operating !== ''" @click="emit('watch', item)">
                {{ logContainer === item.id ? '关闭日志' : '日志' }}
              </NButton>
              <template v-if="item.state === 'running'">
                <NButton
                  quaternary
                  type="primary"
                  size="small"
                  :disabled="!connected || operating !== ''"
                  @click="emit('operate', item, 'stop')"
                >
                  {{ operating === `stop:${item.id}` ? '停止中…' : '停止' }}
                </NButton>
                <NButton
                  quaternary
                  type="primary"
                  size="small"
                  :disabled="!connected || operating !== ''"
                  @click="emit('operate', item, 'restart')"
                >
                  {{ operating === `restart:${item.id}` ? '重启中…' : '重启' }}
                </NButton>
              </template>
              <NButton
                v-else
                quaternary
                type="primary"
                size="small"
                :disabled="!connected || operating !== ''"
                @click="emit('operate', item, 'start')"
              >
                {{ operating === `start:${item.id}` ? '启动中…' : '启动' }}
              </NButton>
              <template v-if="confirmingRemove === item.id">
                <span class="text-xs text-[#667085]">确认删除？不可恢复，数据卷保留。</span>
                <NButton
                  size="small"
                  type="error"
                  :disabled="operating !== ''"
                  @click="emit('operate', item, 'remove')"
                >
                  确认删除
                </NButton>
                <NButton size="small" quaternary @click="emit('cancelRemove')">取消</NButton>
              </template>
              <NButton
                v-else
                quaternary
                type="error"
                size="small"
                :disabled="!connected || operating !== ''"
                :title="connected ? '' : '节点连接中断时操作不可用'"
                @click="emit('askRemove', item)"
              >
                删除
              </NButton>
            </div>
          </td>
        </tr>
        <tr v-if="logContainer && items.some((item) => item.id === logContainer)">
          <td colspan="5" class="bg-[#f7faff] px-4 py-3">
            <p v-if="logBusy" class="py-2 text-[#667085]">正在打开日志…</p>
            <p
              v-if="logError"
              class="mb-2 rounded-lg bg-[#fdeaea] px-4 py-3 text-xs leading-relaxed text-[#e02e2e]"
            >
              {{ logError }}
            </p>
            <LogViewer v-if="!logBusy" :lines="logLines" :truncated="logTruncated" />
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
