<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { NAlert, NButton, NCard, NCheckbox } from 'naive-ui'
import AppShell from '../components/AppShell.vue'
import { api } from '../api/client'
import { createApplication, listApplications } from '../api/deployments'
import { describeError, type AppMode, type AppSummary, type NodeSummary } from '../api/types'

const router = useRouter()

const applications = ref<AppSummary[]>([])
const nodes = ref<NodeSummary[]>([])

// hostnameOf names the target by the name the administrator knows.
//
// The identifier of a node is what the control端 stores, but a person
// recognizes the host. The identifier stays in the title of the cell, so it
// is one hover away when someone needs it.
function hostnameOf(targetID: string): string {
  return nodes.value.find((node) => node.id === targetID)?.hostname ?? targetID
}
const loading = ref(true)
const error = ref('')

// creating reports whether the form of a new application is open.
const creating = ref(false)
const busy = ref(false)
const formError = ref('')
const name = ref('')
const mode = ref<AppMode>('compose')
const targetID = ref('')
const namespace = ref('')
const adopt = ref(false)

// load reads the applications of the control端 and the nodes that can be
// the target of an application.
async function load() {
  loading.value = true
  error.value = ''
  try {
    applications.value = await listApplications()
    const payload = await api<{ nodes: NodeSummary[] }>('/api/v1/nodes')
    nodes.value = payload.nodes
  } catch (failure) {
    error.value = describeError(failure)
  } finally {
    loading.value = false
  }
}

// openForm opens the form of a new application.
function openForm() {
  formError.value = ''
  creating.value = true
}

// closeForm closes the form without creating anything.
function closeForm() {
  creating.value = false
  formError.value = ''
}

// namespaceOf suggests the name of the project of an application.
//
// The name of the project of Compose and of the stack of Swarm may only
// hold small letters, digits and separators.
function namespaceOf(value: string): string {
  return value
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9_-]+/g, '-')
    .replace(/^[^a-z0-9]+/, '')
    .slice(0, 63)
}

// create writes the application the administrator described.
async function create() {
  if (busy.value) {
    return
  }
  busy.value = true
  formError.value = ''
  try {
    const created = await createApplication({
      name: name.value.trim(),
      mode: mode.value,
      target_id: targetID.value.trim(),
      namespace: namespace.value.trim(),
      adopt: adopt.value,
    })
    await router.push(`/applications/${encodeURIComponent(created.id)}`)
  } catch (failure) {
    // The input of the administrator stays in the form, so the reason of
    // the refusal can be fixed without starting over.
    formError.value = describeError(failure)
  } finally {
    busy.value = false
  }
}

onMounted(load)

// The shared face of the native selects of the form. The selects stay
// native: the name of an option is what the tests and the keyboard read.
const selectClass =
  'h-[38px] w-full rounded-lg border border-[#d3dcec] bg-white px-3 text-sm outline-none focus:border-[#025ffb]'
const inputClass =
  'h-[38px] w-full rounded-lg border border-[#d3dcec] bg-white px-3 text-sm outline-none focus:border-[#025ffb]'
</script>

<template>
  <AppShell>
    <div class="mb-1 flex items-center justify-between gap-4">
      <h1 class="page-title">站点管理</h1>
      <div class="flex gap-2">
        <NButton quaternary type="primary" :loading="loading" @click="load">刷新</NButton>
        <NButton type="primary" @click="openForm">新建应用</NButton>
      </div>
    </div>
    <p class="page-subtitle">一个站点以应用的形式管理：一份 Compose / Stack 配置，部署为节点上的一组服务与容器。</p>

    <section class="mb-6">
      <NCard title="应用列表">
        <p v-if="loading" class="py-6 text-[#667085]">正在读取应用…</p>

        <div v-else-if="error" class="rounded-lg bg-[#fdeaea] px-4 py-3 text-xs text-[#e02e2e]">
          <p>{{ error }}</p>
          <NButton quaternary type="primary" class="mt-2" @click="load">重试</NButton>
        </div>

        <template v-else>
          <p v-if="applications.length === 0" class="py-6 text-center text-[#667085]">
            还没有应用。新建一个应用，把 Compose 文件或 Swarm Stack 部署到节点上。
          </p>

          <div v-else class="overflow-x-auto">
            <table class="w-full border-collapse text-left text-[13px]">
              <caption class="sr-only">应用列表</caption>
              <thead>
                <tr>
                  <th scope="col" class="whitespace-nowrap px-5 py-3 text-xs font-medium text-[#667085]">名称</th>
                  <th scope="col" class="whitespace-nowrap px-5 py-3 text-xs font-medium text-[#667085]">模式</th>
                  <th scope="col" class="whitespace-nowrap px-5 py-3 text-xs font-medium text-[#667085]">命名空间</th>
                  <th scope="col" class="whitespace-nowrap px-5 py-3 text-xs font-medium text-[#667085]">目标</th>
                  <th scope="col" class="px-5 py-3"><span class="sr-only">操作</span></th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="app in applications"
                  :key="app.id"
                  class="border-b border-[#e6ecf7] last:border-b-0 hover:bg-[#f7faff]"
                >
                  <th scope="row" class="whitespace-nowrap px-5 py-4 text-[13px] font-medium">
                    <RouterLink :to="`/applications/${encodeURIComponent(app.id)}`" class="hover:text-[#025ffb]">
                      {{ app.name }}
                    </RouterLink>
                  </th>
                  <td class="whitespace-nowrap px-5 py-4">{{ app.mode === 'swarm' ? 'Swarm' : 'Compose' }}</td>
                  <td class="whitespace-nowrap px-5 py-4 font-mono text-xs">{{ app.namespace }}</td>
                  <td class="whitespace-nowrap px-5 py-4" :title="app.target_id">
                    {{ hostnameOf(app.target_id) }}
                  </td>
                  <td class="whitespace-nowrap px-5 py-4 text-right">
                    <NButton quaternary type="primary" size="small" :to="`/applications/${encodeURIComponent(app.id)}`">
                      打开
                    </NButton>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </template>
      </NCard>
    </section>

    <section v-if="creating">
      <NCard title="新建应用">
        <template #header-extra>
          <NButton quaternary @click="closeForm">关闭</NButton>
        </template>

        <form class="grid max-w-2xl gap-4" novalidate @submit.prevent="create">
          <div class="grid gap-1">
            <label class="text-xs font-medium text-[#344054]" for="application-name">名称</label>
            <input
              id="application-name"
              v-model="name"
              :class="inputClass"
              type="text"
              name="application-name"
              required
              @blur="namespace = namespace || namespaceOf(name)"
            />
          </div>

          <div class="grid gap-1">
            <label class="text-xs font-medium text-[#344054]" for="application-mode">模式</label>
            <select id="application-mode" v-model="mode" :class="selectClass" name="application-mode">
              <option value="compose">Compose</option>
              <option value="swarm">Swarm</option>
            </select>
            <p class="text-xs text-[#667085]">模式在创建之后不能更改。</p>
          </div>

          <div class="grid gap-1">
            <label class="text-xs font-medium text-[#344054]" for="application-target">目标</label>
            <select
              v-if="mode === 'compose'"
              id="application-target"
              v-model="targetID"
              :class="selectClass"
              name="application-target"
            >
              <option value="">请选择节点</option>
              <option v-for="node in nodes" :key="node.id" :value="node.id">
                {{ node.hostname }}（{{ node.id }}）
              </option>
            </select>
            <input
              v-else
              id="application-target"
              v-model="targetID"
              :class="inputClass"
              type="text"
              name="application-target"
            />
            <p class="text-xs text-[#667085]">
              {{
                mode === 'compose'
                  ? 'Compose 应用运行在一个节点上。'
                  : 'Swarm 应用运行在一个集群上，请填写集群的标识。'
              }}
            </p>
          </div>

          <div class="grid gap-1">
            <label class="text-xs font-medium text-[#344054]" for="application-namespace">命名空间</label>
            <input
              id="application-namespace"
              v-model="namespace"
              :class="inputClass"
              type="text"
              name="application-namespace"
              required
            />
            <p class="text-xs text-[#667085]">
              命名空间是 Compose 项目和 Swarm Stack 的名字，只能包含小写字母、数字、- 和 _。
            </p>
          </div>

          <div class="grid gap-1">
            <NCheckbox v-model:checked="adopt">我确认接管该命名空间下已有的资源</NCheckbox>
            <p class="text-xs text-[#667085]">
              只有在该命名空间里已有平台不管理的资源时，才需要勾选这一项。
            </p>
          </div>

          <NAlert v-if="formError" type="error" :show-icon="false">{{ formError }}</NAlert>

          <NButton class="self-start" type="primary" attr-type="submit" :loading="busy">
            创建应用
          </NButton>
        </form>
      </NCard>
    </section>
  </AppShell>
</template>
