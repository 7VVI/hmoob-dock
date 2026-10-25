<script setup lang="ts">
import { onMounted, ref } from 'vue'
import AppShell from '../components/AppShell.vue'
import NodeTable from '../components/NodeTable.vue'
import { api } from '../api/client'
import { describeError, type NodeListPayload, type NodeSummary } from '../api/types'

const nodes = ref<NodeSummary[]>([])
const loading = ref(true)
const error = ref('')

// load reads the nodes of the deployment.
//
// The number of the nodes is shown only when the list of the API arrived,
// so the page never counts nodes that it does not know.
async function load() {
  loading.value = true
  error.value = ''
  try {
    const payload = await api<NodeListPayload>('/api/v1/nodes')
    nodes.value = payload.nodes
  } catch (failure) {
    error.value = describeError(failure)
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <AppShell>
    <h1 class="page__title">概览</h1>

    <section class="panel">
      <div class="panel__head">
        <h2 class="panel__title">节点</h2>
        <button type="button" class="button button--quiet" :disabled="loading" @click="load">
          刷新
        </button>
      </div>

      <p v-if="loading" class="muted">正在读取节点…</p>

      <div v-else-if="error" class="alert" role="alert">
        <p>{{ error }}</p>
        <button type="button" class="button button--quiet" @click="load">重试</button>
      </div>

      <template v-else>
        <p v-if="nodes.length > 0" class="muted">共 {{ nodes.length }} 个节点。</p>
        <NodeTable :nodes="nodes" />
        <p v-if="nodes.length === 0" class="muted">
          在设置页生成一次性接入令牌，可以让第一个 Agent 接入。
        </p>
      </template>
    </section>
  </AppShell>
</template>
