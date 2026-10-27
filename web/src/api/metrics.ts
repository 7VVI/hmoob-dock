// The client of the history of the metrics of a container.
//
// The history of a container belongs to the node that runs it, so a request
// names the node and the container together. The control端 answers with the
// minutes it observed: a minute that was not observed has no bucket at all,
// and the reader fills the gap with an empty value instead of a zero.
//
// Every call of this module goes to the control端 of the same origin, so
// the session of the administrator travels in the cookie of the browser and
// no token ever appears in the address of a request.
import { api } from './client'
import type { MetricBucket, MetricPoint, MetricsPayload } from './types'

// MetricName is the name of a value of a sample of a container.
//
// The names are the names that the API uses, so a document of the API can be
// read without a translation.
export type MetricName =
  | 'cpu_percent'
  | 'memory_bytes'
  | 'net_rx_per_sec'
  | 'net_tx_per_sec'
  | 'io_read_per_sec'
  | 'io_write_per_sec'

// metricNames names every metric the platform measures.
//
// The label is the name the administrator reads: the curve belongs to a
// container, never to the whole machine.
export const metricNames: { name: MetricName; label: string }[] = [
  { name: 'cpu_percent', label: 'CPU' },
  { name: 'memory_bytes', label: '内存' },
  { name: 'net_rx_per_sec', label: '网络接收' },
  { name: 'net_tx_per_sec', label: '网络发送' },
  { name: 'io_read_per_sec', label: '磁盘读取' },
  { name: 'io_write_per_sec', label: '磁盘写入' },
]

// The limits of the history of the metrics.
const minute = 60 * 1000

// staleAfter is the time the newest observation may be old before the page
// tells the administrator that the data is out of date.
const staleAfter = 3 * minute

// MetricsQuery names the container and the window of a history request.
export interface MetricsQuery {
  node_id: string
  container_id: string
  // from is the start of the window in UTC.
  from: string
  // to is the end of the window in UTC.
  to: string
  limit?: number
}

// getMetrics reads the minute history of a container.
//
// The window may not be longer than a day: the history is kept for the same
// span, so a longer request would only hide that the older data is gone.
export async function getMetrics(query: MetricsQuery): Promise<MetricsPayload> {
  const params = new URLSearchParams()
  params.set('node_id', query.node_id)
  params.set('container_id', query.container_id)
  params.set('from', query.from)
  params.set('to', query.to)
  if (query.limit !== undefined) {
    params.set('limit', String(query.limit))
  }
  return api<MetricsPayload>(`/api/v1/metrics?${params.toString()}`)
}

// metricPoints builds the points of a curve.
//
// Every minute of the window gets a point, so the time line of the curve
// holds every minute. A minute that the control端 did not observe holds no
// value: the gap stays visible and the reader never invents a number for it.
export function metricPoints(
  buckets: MetricBucket[],
  metric: MetricName,
  from: string,
  to: string,
): MetricPoint[] {
  const start = Date.parse(from)
  const end = Date.parse(to)
  if (Number.isNaN(start) || Number.isNaN(end) || end <= start) {
    return []
  }

  const observed = new Map<number, MetricBucket>()
  for (const bucket of buckets) {
    const at = Date.parse(bucket.start)
    if (Number.isNaN(at)) {
      continue
    }
    observed.set(Math.floor(at / minute) * minute, bucket)
  }

  const points: MetricPoint[] = []
  for (let at = Math.floor(start / minute) * minute; at < end; at += minute) {
    const bucket = observed.get(at)
    points.push({
      at: new Date(at).toISOString(),
      average: bucket ? (bucket.average[metric] ?? null) : null,
      peak: bucket ? (bucket.peak[metric] ?? null) : null,
      complete: bucket ? bucket.complete : false,
    })
  }
  return points
}

// metricUnit names the unit of a metric.
//
// The CPU is measured against the cores of the machine, so a value of a
// hundred means one core. The other metrics are measured in bytes, so they
// are shown in the same unit everywhere.
export function metricUnit(metric: MetricName): string {
  switch (metric) {
    case 'cpu_percent':
      return '每核 100%'
    case 'memory_bytes':
      return 'MiB'
    default:
      return 'KiB/s'
  }
}

// metricScale writes the value of a metric in the unit of its curve.
//
// The unit itself is named beside the curve, so the scale holds the number
// alone.
export function metricScale(metric: MetricName): (value: number) => string {
  switch (metric) {
    case 'cpu_percent':
      return (value) => value.toFixed(1)
    case 'memory_bytes':
      return (value) => (value / 1024 / 1024).toFixed(1)
    default:
      return (value) => (value / 1024).toFixed(1)
  }
}

// formatMetricValue writes a value of a metric for the page.
export function formatMetricValue(value: number, metric: MetricName): string {
  return `${metricScale(metric)(value)} ${metricUnit(metric)}`
}

// chartSeries builds the two lines of a curve.
//
// The lines never join over a gap: a minute that was not observed holds no
// value, and the curve shows the gap instead of a line that never existed.
export function chartSeries(points: MetricPoint[]): {
  name: string
  type: 'line'
  connectNulls: false
  data: (string | number | null)[][]
}[] {
  return [
    {
      name: '平均值',
      type: 'line',
      connectNulls: false,
      data: points.map((point) => [point.at, point.average]),
    },
    {
      name: '峰值',
      type: 'line',
      connectNulls: false,
      data: points.map((point) => [point.at, point.peak]),
    },
  ]
}

// newestPoint returns the newest point that holds a value.
export function newestPoint(points: MetricPoint[]): MetricPoint | null {
  for (let index = points.length - 1; index >= 0; index -= 1) {
    const point = points[index]
    if (point.average !== null || point.peak !== null) {
      return point
    }
  }
  return null
}

// isStale reports whether the newest observation is too old to be current.
//
// The control端 writes a minute as soon as it is complete, so a data set
// whose newest minute is much older than that belongs to a container that
// nobody watches any more.
export function isStale(points: MetricPoint[], now: number = Date.now()): boolean {
  const newest = newestPoint(points)
  if (!newest) {
    return true
  }
  const at = Date.parse(newest.at)
  if (Number.isNaN(at)) {
    return true
  }
  return now - at > staleAfter
}

// ContainerSummary is one container of a node, as the page names it.
export interface ContainerSummary {
  id: string
  name: string
  image: string
  state: string
  health: string
  service: string
}

// listContainers reads the containers of a node.
//
// The list comes from the last complete inventory of the node, so the page
// can offer the containers by name instead of asking for an identifier.
export async function listContainers(nodeID: string): Promise<ContainerSummary[]> {
  const payload = await api<{ containers: ContainerSummary[] }>(
    `/api/v1/nodes/${encodeURIComponent(nodeID)}/containers`,
  )
  return payload.containers ?? []
}
