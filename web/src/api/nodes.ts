// The client of the nodes and of the operations on their containers.
//
// A node is an Agent endpoint: its record is the enrollment of the
// control端, its live state comes from the connection of its agent and its
// containers come from the last inventory the agent reported. An operation
// on a container runs on the node itself, so its answer carries the state
// the node read after the operation, or the reason the node refused.
//
// Every call of this module goes to the control端 of the same origin, so
// the session of the administrator travels in the cookie of the browser and
// no token ever appears in the address of a request.
import { api } from './client'
import type {
  ContainerAction,
  ContainerOpResult,
  NodeDetailPayload,
  NodeListPayload,
  NodeSummary,
} from './types'

// listNodes reads the enrolled nodes and their live state.
export async function listNodes(): Promise<NodeSummary[]> {
  const payload = await api<NodeListPayload>('/api/v1/nodes')
  return payload.nodes ?? []
}

// getNode reads the record and the live state of one node.
export async function getNode(nodeID: string): Promise<NodeSummary> {
  const payload = await api<NodeDetailPayload>(
    `/api/v1/nodes/${encodeURIComponent(nodeID)}`,
  )
  return payload.node
}

// containerOp performs one operation on one container of a node.
//
// The answer of the control端 is the answer of the node. A refusal of the
// platform (a container of an application, a deployment that runs) and a
// refusal of the node both arrive as an ApiError; an operation whose result
// never arrived arrives as a timeout error, and the page tells the
// administrator to read the inventory again instead of claiming a result.
export async function containerOp(
  nodeID: string,
  containerID: string,
  action: ContainerAction,
): Promise<ContainerOpResult> {
  return api<{ result: ContainerOpResult }>(
    `/api/v1/nodes/${encodeURIComponent(nodeID)}/containers/${encodeURIComponent(containerID)}/op`,
    { method: 'POST', body: JSON.stringify({ action }) },
  ).then((payload) => payload.result)
}

// roleLabel names the role of a node the way the administrator reads it.
export function roleLabel(role: string): string {
  switch (role) {
    case 'manager':
      return 'Swarm Manager'
    case 'worker':
      return 'Swarm Worker'
    default:
      return '独立主机'
  }
}

// describeOpError turns the error of an operation into a sentence the page
// can show. The error codes are the ones the control端 answers with.
export function describeOpError(error: unknown): string {
  const code = (error as { code?: string })?.code
  const message = (error as { message?: string })?.message
  switch (code) {
    case 'target_unavailable':
      return message || '节点当前没有连接，操作不可用。'
    case 'managed_by_application':
      return message || '这个容器由站点管理，删除请编辑站点 YAML 并重新部署。'
    case 'deployment_in_progress':
      return message || '该站点有一个未结束的部署，容器操作暂不可用。'
    case 'operation_timeout':
      return message || '节点没有在期限内回答，操作可能仍在执行，请刷新清单核对。'
    case 'container_not_found':
      return message || '节点最近的清单里没有这个容器。'
    default:
      return message || '操作失败，请稍后重试。'
  }
}
