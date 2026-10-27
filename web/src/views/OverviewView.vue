<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import AppShell from '../components/AppShell.vue'
import NodeTable from '../components/NodeTable.vue'
import MetricChart from '../components/MetricChart.vue'
import { api } from '../api/client'
import {
  describeError,
  formatTimestamp,
  type NodeListPayload,
  type NodeSummary,
} from '../api/types'
import {
  getMetrics,
  isStale,
  metricNames,
  metricPoints,
  metricScale,
  metricUnit,
  newestPoint,
  listContainers,
  type ContainerSummary,
  type MetricName,
} from '../api/metrics'

const nodes = ref<NodeSummary[]>([])
const loading = ref(true)
const error = ref('')

// The window of the curve.
//
// The control端 keeps the history for a day, so a longer window would only
// hide that the older data is gone.
const ranges = [
  { minutes: 60, label: '最近 1 小时' },
  { minutes: 360, label: '最近 6 小时' },
  { minutes: 1440, label: '最近 24 小时' },
]

const nodeID = ref('')
const containerID = ref('')
const containers = ref<ContainerSummary[]>([])
const containersError = ref('')
const metric = ref<MetricName>('cpu_percent')
const range = ref(ranges[0].minutes)

const points = ref<ReturnType<typeof metricPoints>>([])
const curveLoading = ref(false)
const curveError = ref('')
const curveLoaded = ref(false)

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
    if (nodeID.value === '' && payload.nodes.length > 0) {
      nodeID.value = payload.nodes[0].id
    }
  } catch (failure) {
    error.value = describeError(failure)
  } finally {
    loading.value = false
  }
}

// theNode is the node of the curve.
const theNode = computed(() => nodes.value.find((node) => node.id === nodeID.value) ?? null)

// theNodeIsConnected reports whether the agent of the node is connected.
const theNodeIsConnected = computed(() => theNode.value?.connected === true)

// containerCount is the number of the containers of the deployment.
const containerCount = computed(() =>
  nodes.value.reduce((total, node) => total + node.container_count, 0),
)

// theMetricName names the metric of the curve.
const theMetricName = computed(
  () => metricNames.find((item) => item.name === metric.value)?.label ?? metric.value,
)

// theUnit is the unit of the curve.
const theUnit = computed(() => metricUnit(metric.value))

// theScale writes a value of the curve in its unit.
const theScale = computed(() => metricScale(metric.value))

// theDataIsStale reports whether the newest observation is too old.
const theDataIsStale = computed(() => curveLoaded.value && isStale(points.value))

// theNewestAt is the moment of the newest observation of the curve.
const theNewestAt = computed(() => newestPoint(points.value)?.at ?? '')

// loadCurve reads the history of the container.
//
// A container that is not named has no history to read, so the page waits
// for the input of the administrator.
async function loadCurve(): Promise<void> {
  if (nodeID.value === '' || containerID.value.trim() === '') {
    points.value = []
    curveLoaded.value = false
    curveError.value = ''
    return
  }
  curveLoading.value = true
  curveError.value = ''
  try {
    const to = new Date()
    const from = new Date(to.getTime() - range.value * 60 * 1000)
    const payload = await getMetrics({
      node_id: nodeID.value,
      container_id: containerID.value.trim(),
      from: from.toISOString(),
      to: to.toISOString(),
    })
    points.value = metricPoints(payload.buckets, metric.value, payload.from, payload.to)
    curveLoaded.value = true
  } catch (failure) {
    points.value = []
    curveLoaded.value = false
    curveError.value = describeError(failure)
  } finally {
    curveLoading.value = false
  }
}

// loadContainers reads the containers of the node of the curve.
//
// The list names the containers by their name, so the administrator picks
// one instead of typing an identifier. A running container comes first,
// because it is the one whose metrics tell something.
async function loadContainers(): Promise<void> {
  containersError.value = ''
  if (nodeID.value === '') {
    containers.value = []
    containerID.value = ''
    return
  }
  try {
    const list = await listContainers(nodeID.value)
    containers.value = list
    if (!list.some((item) => item.id === containerID.value)) {
      containerID.value = list.find((item) => item.state === 'running')?.id ?? list[0]?.id ?? ''
    }
  } catch (failure) {
    containers.value = []
    containerID.value = ''
    containersError.value = describeError(failure)
  }
}

// chooseNode shows the curve of the container of a node.
//
// The container is named by the administrator, so the page only prepares
// the node and waits for the container.
function chooseNode(node: NodeSummary): void {
  nodeID.value = node.id
  curveLoaded.value = false
  points.value = []
}

onMounted(load)

// The containers belong to the node of the curve, so the list follows the
// choice of the node.
watch(nodeID, () => {
  void loadContainers()
})
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
        <p v-if="nodes.length > 0" class="muted">
          共 {{ nodes.length }} 个节点，{{ containerCount }} 个容器。
        </p>
        <NodeTable :nodes="nodes">
          <template #actions="{ node }">
            <button type="button" class="button button--quiet" @click="chooseNode(node)">
              查看资源曲线
            </button>
          </template>
        </NodeTable>
        <p v-if="nodes.length === 0" class="muted">
          在设置页生成一次性接入令牌，可以让第一个 Agent 接入。
        </p>
      </template>
    </section>

    <section class="panel">
      <h2 class="panel__title">资源曲线</h2>
      <p class="muted">曲线是容器的，不是整台机器的。</p>

      <form class="form form--inline" novalidate @submit.prevent="loadCurve">
        <div class="field">
          <label class="field__label" for="curve-node">节点</label>
          <select id="curve-node" v-model="nodeID" class="field__input" name="curve-node">
            <option v-for="node in nodes" :key="node.id" :value="node.id">
              {{ node.hostname }}
            </option>
          </select>
        </div>

        <div class="field">
          <label class="field__label" for="curve-container">容器</label>
          <select
            id="curve-container"
            v-model="containerID"
            class="field__input"
            name="curve-container"
            :disabled="containers.length === 0"
          >
            <option v-if="containers.length === 0" value="">这个节点还没有上报容器</option>
            <option v-for="item in containers" :key="item.id" :value="item.id">
              {{ item.name }} · {{ item.state }}{{ item.health ? ' · ' + item.health : '' }}
            </option>
          </select>
          <p v-if="containersError" class="alert" role="alert">{{ containersError }}</p>
        </div>

        <div class="field">
          <label class="field__label" for="curve-metric">指标</label>
          <select id="curve-metric" v-model="metric" class="field__input" name="curve-metric">
            <option v-for="item in metricNames" :key="item.name" :value="item.name">
              {{ item.label }}
            </option>
          </select>
        </div>

        <div class="field">
          <label class="field__label" for="curve-range">时间范围</label>
          <select id="curve-range" v-model.number="range" class="field__input" name="curve-range">
            <option v-for="item in ranges" :key="item.minutes" :value="item.minutes">
              {{ item.label }}
            </option>
          </select>
        </div>

        <button class="button button--primary" type="submit" :disabled="curveLoading">
          {{ curveLoading ? '读取中…' : '查看曲线' }}
        </button>
      </form>

      <p v-if="curveError" class="alert" role="alert">
        <span>{{ curveError }}</span>
        <button type="button" class="button button--quiet" @click="loadCurve">重试</button>
      </p>

      <p v-if="theNode && !theNodeIsConnected" class="notice">
        连接中断。曲线停在最后一次收到数据的时候。
      </p>

      <p v-else-if="theDataIsStale" class="notice">
        数据已过期。最后一次数据来自
        {{ theNewestAt === '' ? '更早' : formatTimestamp(theNewestAt) }}。
      </p>

      <p v-if="!curveLoaded && !curveLoading && curveError === ''" class="empty">
        选好容器和指标后点「查看曲线」。
      </p>

      <MetricChart
        v-if="curveLoaded"
        :points="points"
        :unit="theUnit"
        :title="theMetricName"
        :format="theScale"
      />
    </section>
  </AppShell>
</template>

<style scoped>
.form--inline {
  grid-template-columns: repeat(auto-fit, minmax(11rem, 1fr));
  align-items: end;
  max-width: none;
}

.form--inline .button {
  margin-bottom: var(--space-1);
}
</style>
