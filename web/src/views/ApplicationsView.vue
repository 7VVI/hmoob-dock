<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import AppShell from '../components/AppShell.vue'
import { api } from '../api/client'
import { createApplication, listApplications } from '../api/deployments'
import { describeError, type AppMode, type AppSummary, type NodeSummary } from '../api/types'

const router = useRouter()

const applications = ref<AppSummary[]>([])
const nodes = ref<NodeSummary[]>([])
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
</script>

<template>
  <AppShell>
    <h1 class="page__title">应用</h1>

    <section class="panel">
      <div class="panel__head">
        <h2 class="panel__title">应用列表</h2>
        <div class="panel__actions">
          <button type="button" class="button button--quiet" :disabled="loading" @click="load">
            刷新
          </button>
          <button type="button" class="button button--primary" @click="openForm">新建应用</button>
        </div>
      </div>

      <p v-if="loading" class="muted">正在读取应用…</p>

      <div v-else-if="error" class="alert" role="alert">
        <p>{{ error }}</p>
        <button type="button" class="button button--quiet" @click="load">重试</button>
      </div>

      <template v-else>
        <p v-if="applications.length === 0" class="empty">
          还没有应用。新建一个应用，把 Compose 文件或 Swarm Stack 部署到节点上。
        </p>

        <div v-else class="table-scroll">
          <table class="nodes">
            <caption class="visually-hidden">应用列表</caption>
            <thead>
              <tr>
                <th scope="col">名称</th>
                <th scope="col">模式</th>
                <th scope="col">命名空间</th>
                <th scope="col">目标</th>
                <th scope="col">
                  <span class="visually-hidden">操作</span>
                </th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="app in applications" :key="app.id">
                <th scope="row" class="nodes__host">
                  <RouterLink class="history__link" :to="`/applications/${encodeURIComponent(app.id)}`">
                    {{ app.name }}
                  </RouterLink>
                </th>
                <td>{{ app.mode === 'swarm' ? 'Swarm' : 'Compose' }}</td>
                <td class="nodes__host">{{ app.namespace }}</td>
                <td class="nodes__host">{{ app.target_id }}</td>
                <td class="nodes__actions">
                  <RouterLink
                    class="button button--quiet"
                    :to="`/applications/${encodeURIComponent(app.id)}`"
                  >
                    打开
                  </RouterLink>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
    </section>

    <section v-if="creating" class="panel">
      <div class="panel__head">
        <h2 class="panel__title">新建应用</h2>
        <button type="button" class="button button--quiet" @click="closeForm">关闭</button>
      </div>

      <form class="form" novalidate @submit.prevent="create">
        <div class="field">
          <label class="field__label" for="application-name">名称</label>
          <input
            id="application-name"
            v-model="name"
            class="field__input"
            type="text"
            name="application-name"
            required
            @blur="namespace = namespace || namespaceOf(name)"
          />
        </div>

        <div class="field">
          <label class="field__label" for="application-mode">模式</label>
          <select
            id="application-mode"
            v-model="mode"
            class="field__input"
            name="application-mode"
          >
            <option value="compose">Compose</option>
            <option value="swarm">Swarm</option>
          </select>
          <p class="field__hint">模式在创建之后不能更改。</p>
        </div>

        <div class="field">
          <label class="field__label" for="application-target">目标</label>
          <select
            v-if="mode === 'compose'"
            id="application-target"
            v-model="targetID"
            class="field__input"
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
            class="field__input"
            type="text"
            name="application-target"
          />
          <p class="field__hint">
            {{
              mode === 'compose'
                ? 'Compose 应用运行在一个节点上。'
                : 'Swarm 应用运行在一个集群上，请填写集群的标识。'
            }}
          </p>
        </div>

        <div class="field">
          <label class="field__label" for="application-namespace">命名空间</label>
          <input
            id="application-namespace"
            v-model="namespace"
            class="field__input"
            type="text"
            name="application-namespace"
            required
          />
          <p class="field__hint">
            命名空间是 Compose 项目和 Swarm Stack 的名字，只能包含小写字母、数字、- 和 _。
          </p>
        </div>

        <div class="field">
          <label class="field__check">
            <input v-model="adopt" type="checkbox" name="application-adopt" />
            <span>我确认接管该命名空间下已有的资源</span>
          </label>
          <p class="field__hint">
            只有在该命名空间里已有平台不管理的资源时，才需要勾选这一项。
          </p>
        </div>

        <p v-if="formError" class="alert" role="alert">{{ formError }}</p>

        <button class="button button--primary" type="submit" :disabled="busy">
          {{ busy ? '创建中…' : '创建应用' }}
        </button>
      </form>
    </section>
  </AppShell>
</template>

<style scoped>
.panel__actions {
  display: flex;
  gap: var(--space-2);
}

.field__check {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: 0.9rem;
}
</style>
