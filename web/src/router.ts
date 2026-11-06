import { createRouter, createWebHistory } from 'vue-router'
import { api } from './api/client'
import type { SessionPayload } from './api/types'
import { restoreSession, sessionState, type SessionStatus } from './auth/session'
import ApplicationDetailView from './views/ApplicationDetailView.vue'
import ApplicationsView from './views/ApplicationsView.vue'
import AlertsView from './views/AlertsView.vue'
import ContainerDetailView from './views/ContainerDetailView.vue'
import InitializeView from './views/InitializeView.vue'
import LoginView from './views/LoginView.vue'
import NodeDetailView from './views/NodeDetailView.vue'
import NodeListView from './views/NodeListView.vue'
import OverviewView from './views/OverviewView.vue'
import ServicesView from './views/ServicesView.vue'
import SettingsView from './views/SettingsView.vue'

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', name: 'overview', component: OverviewView },
    { path: '/initialize', name: 'initialize', component: InitializeView },
    { path: '/login', name: 'login', component: LoginView },
    { path: '/settings', name: 'settings', component: SettingsView },
    { path: '/applications', name: 'applications', component: ApplicationsView },
    { path: '/applications/:id', name: 'application', component: ApplicationDetailView },
    { path: '/services', name: 'services', component: ServicesView },
    { path: '/nodes', name: 'nodes', component: NodeListView },
    { path: '/nodes/:id/containers/:cid', name: 'container', component: ContainerDetailView },
    { path: '/nodes/:id', name: 'node', component: NodeDetailView },
    { path: '/alerts', name: 'alerts', component: AlertsView },
  ],
})

// currentStatus reads the state of the session.
//
// The answer of the API stays in the memory of the page, so the state is
// asked only once per page.
async function currentStatus(): Promise<SessionStatus> {
  if (sessionState.status !== 'unknown') {
    return sessionState.status
  }
  return restoreSession(() => api<SessionPayload>('/api/v1/auth/session'))
}

// The guard sends the administrator to the page that fits the state of the
// deployment: the initialization while no administrator exists, the login
// while the page holds no session and the overview otherwise.
router.beforeEach(async (to) => {
  const status = await currentStatus()
  if (status === 'uninitialized') {
    return to.path === '/initialize' ? true : { path: '/initialize' }
  }
  if (status !== 'authenticated') {
    return to.path === '/login' ? true : { path: '/login' }
  }
  return to.path === '/login' ? { path: '/' } : true
})
