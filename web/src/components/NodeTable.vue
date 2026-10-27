<script setup lang="ts">
import { formatTimestamp, type NodeSummary } from '../api/types'

defineProps<{ nodes: NodeSummary[] }>()
</script>

<template>
  <p v-if="nodes.length === 0" class="empty">还没有接入任何节点。</p>
  <div v-else class="table-scroll">
    <table class="nodes">
      <caption class="visually-hidden">已接入的节点</caption>
      <thead>
        <tr>
          <th scope="col">主机名</th>
          <th scope="col">状态</th>
          <th scope="col">容器</th>
          <th scope="col">最近活动</th>
          <th v-if="$slots.actions" scope="col">
            <span class="visually-hidden">操作</span>
          </th>
        </tr>
      </thead>
      <tbody>
        <tr
          v-for="node in nodes"
          :key="node.id"
          :data-state="node.connected ? 'online' : 'offline'"
        >
          <th scope="row" class="nodes__host">{{ node.hostname }}</th>
          <td>
            <span class="state" :class="node.connected ? 'state--online' : 'state--offline'">
              {{ node.connected ? '已连接' : '连接中断' }}
            </span>
          </td>
          <td class="nodes__count">{{ node.container_count }}</td>
          <td class="nodes__seen">{{ formatTimestamp(node.last_seen) }}</td>
          <td v-if="$slots.actions" class="nodes__actions">
            <slot name="actions" :node="node" />
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
