# 阶段三：监控、告警与完整验收 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在真实应用部署能力上补齐实时资源监控、24 小时曲线、按需日志、告警与 Webhook，并完成完整首版验收。

**Architecture:** Agent 按固定间隔采集容器指标与状态事件，经既有 WSS 有界传输。控制端立即判断告警并向页面发送实时事件，按分钟聚合存 SQLite；通知独立工作循环持久化重试，不阻塞部署。

**Tech Stack:** 已建立的 Go/SQLite/WSS/Vue 基础、Docker SDK stats/events/logs、ECharts、Vitest、Playwright；不增加独立时序库、消息队列或告警服务。

**Spec:** [已批准设计](../specs/2026-10-24-hmoob-dock-design.md)。前置：[阶段一](2026-10-25-01-control-and-agent.md)与[阶段二](2026-10-25-02-deployment.md)完成。

## Global Constraints

- 单管理员；不增加 Kubernetes、多租户、外部数据库、Prometheus、Alertmanager、Redis。
- 指标采集间隔：10 秒；历史聚合粒度与保留：1 分钟，24 小时。
- CPU 按每核 100% 的口径展示，多核使用可超过 100%；内存采用 Docker API 上报的当前使用字节数。
- Agent 心跳与失联判断：10 秒心跳，持续 60 秒未收到有效心跳判定失联。
- 单任务已保存输出上限：2 MiB，超限截断并提示。
- 已结束部署、告警和审计记录：默认保留 90 天。
- 配置版本：每应用保留最近 50 个；正在执行和最近成功版本不因数量上限删除，历史部署引用同样保护。
- Webhook 重试：初次失败后最多重试 3 次，采用退避间隔。
- 资源告警阈值：由管理员显式配置，不凭空采用统一生产阈值。
- 断线不补零；缺失数据不是恢复，也不是持续超限。控制端停机期间不承诺采集或通知。
- 不长期集中保存容器业务日志；临时日志纯文本显示、按需订阅、有界内存。
- 所有表使用真实 SQLite 测试，浏览器与真实 Linux Compose/Swarm 是完整验收必需条件。
- 当前没有 Git 初始化/提交/推送授权，也没有操作生产服务器授权。

---

## 文件边界与执行顺序

| 文件/目录 | 职责 |
| --- | --- |
| `internal/protocol/telemetry.go` | 原始采样、容器事件、日志请求/帧 |
| `internal/agent/telemetry.go`, `logs.go` | 有界采集与订阅生命周期 |
| `internal/metrics/` | 指标口径、时钟/计数器校验、分钟聚合、历史查询 |
| `internal/alerts/` | 规则、持续条件、事件合并、恢复和确认 |
| `internal/notify/` | Webhook URL 与连接目标校验、发送和持久化重试 |
| `internal/retention/` | 分批清理、不删除被引用版本、保护未结束事件 |
| `internal/httpapi/metrics.go`, `alerts.go`, `logs.go` | 管理员授权的查询与日志订阅 |
| `web/src/components/MetricChart.vue`, `LogViewer.vue` | 曲线缺口与纯文本日志 |
| `web/src/views/AlertsView.vue` | 规则、事件、通知结果 |
| `tests/integration/observability_test.go`, `acceptance_test.go` | 隔离环境的真实故障与完整验收 |

顺序：3.1 → 3.2；3.1 → 3.3 → 3.4；3.1–3.4 → 3.5；所有任务 → 3.6。每个任务保留自己的测试门槛。

## Task 3.1: 容器采样、事件与按需日志传输

**Files:**
- Create: `internal/protocol/telemetry.go`, `internal/agent/telemetry.go`, `internal/agent/logs.go`, `internal/metrics/calculate.go`。
- Create: `internal/dockerops/stats.go`, `internal/dockerops/logs.go`, `internal/httpapi/logs.go`。
- Modify: `internal/agent/runtime.go`, `internal/agentlink/hub.go`, `internal/httpapi/events.go`。
- Test: `internal/metrics/calculate_test.go`, `internal/agent/logs_test.go`, `internal/agent/telemetry_test.go`, `internal/httpapi/logs_test.go`。

**Interfaces:**

```go
type RawSample struct {
    ContainerID string `json:"container_id"`
    CollectedAt time.Time `json:"collected_at"`
    CPUUsageNS uint64 `json:"cpu_usage_ns"`
    SystemUsageNS uint64 `json:"system_usage_ns"`
    OnlineCPUs uint32 `json:"online_cpus"`
    MemoryUsage uint64 `json:"memory_usage"`
    MemoryLimit uint64 `json:"memory_limit"`
    NetRX uint64 `json:"net_rx"`
    NetTX uint64 `json:"net_tx"`
    IORead uint64 `json:"io_read"`
    IOWrite uint64 `json:"io_write"`
}
type ContainerEvent struct {
    ContainerID, ServiceID, Slot, NodeID, Action string
    ExitCode *int
    OccurredAt time.Time
}
type LogRequest struct {
    SubscriptionID, ContainerID string
    Tail int
    Follow bool
}
type LogFrame struct {
    SubscriptionID string
    Offset uint64
    Text string
    Truncated bool
    ErrorCode string
}
```

以上类型在 `protocol` 包。来源控制端 node ID 来自已认证连接，不直接信任事件中的 NodeID。

- Produces: `metrics.Values{CPUPercent, MemoryBytes, NetRXPerSec, NetTXPerSec, IOReadPerSec, IOWritePerSec float64}`；`metrics.Derived{Values metrics.Values; Valid bool}`；`metrics.Calculate(previous, current protocol.RawSample) metrics.Derived`。
- Produces: `(*dockerops.Local).Sample(ctx context.Context, containerID string) (protocol.RawSample, error)`；`Events(ctx context.Context, emit func(protocol.ContainerEvent) error) error`；`Logs(ctx context.Context, req protocol.LogRequest, emit func(protocol.LogFrame) error) error`。

- [ ] **Step 1: 编写计数器重置失败测试。**

```go
func TestCounterResetCreatesGapInsteadOfNegativeRate(t *testing.T) {
    previous := protocol.RawSample{ContainerID: "c1", CollectedAt: time.Unix(100, 0), NetRX: 1000, CPUUsageNS: 100, SystemUsageNS: 1000, OnlineCPUs: 2}
    current := protocol.RawSample{ContainerID: "c1", CollectedAt: time.Unix(110, 0), NetRX: 10, CPUUsageNS: 200, SystemUsageNS: 2000, OnlineCPUs: 2}
    result := metrics.Calculate(previous, current)
    if result.Valid { t.Fatalf("counter reset accepted: %+v", result) }
}
```

- [ ] **Step 2: 执行 RED。** `go test ./internal/metrics ./internal/agent -run 'Counter|Sample|Log' -count=1`。
- [ ] **Step 3: 实现受限采集与准确口径。** 首个样本、容器 ID 变化、非递增时间、计数器下降、system delta 零均返回无效采样；不能把它变成 0。有效差值先检查再做 uint64 相减：

```go
elapsed := current.CollectedAt.Sub(previous.CollectedAt).Seconds()
cpu := float64(current.CPUUsageNS-previous.CPUUsageNS) /
    float64(current.SystemUsageNS-previous.SystemUsageNS) * float64(current.OnlineCPUs) * 100
netRX := float64(current.NetRX-previous.NetRX) / elapsed
```

其余速率同一时间基准；内存直接取使用字节数，限制来自容器显式配置，未限额不冒充“0 字节上限”。Agent 每 10 秒对本机清单有界并发采样（默认最多 8 个请求），单容器超时不拖死整轮；不持久化待补传历史指标。

Docker events 独立订阅，不能靠 10 秒轮询漏掉快速退出；重连用清单校准当前状态，不伪造掉线期间完整事件历史。

协议增加 `metrics.sample`、`container.event`、`logs.subscribe`、`logs.unsubscribe`、`logs.frame`。日志 tail 默认 200、上限 2000 行；每帧最多 32 KiB，每订阅最多 256 KiB 队列；慢消费者丢弃尾随片段时明确 truncated，不阻塞控制回执。正确解码 Docker stdout/stderr multiplex，TTY 流不按 multiplex 解析。

浏览器 `POST /log-subscriptions` 创建仅属当前会话的订阅，`DELETE /log-subscriptions/{id}` 撤销；通过现有 SSE 接收帧。创建时校验节点与容器归属，不允许浏览器提供任意目标 Agent。SSE 断开或会话过期时注销其日志源；多个页面使用引用计数，只在最后订阅关闭后取消共享读取。

- [ ] **Step 4: 执行 GREEN。** 验证 CPU 可超过 100%、计数器重置、时钟异常、容器替换、重连无补零、日志驱动不支持、容器删除、慢订阅、关闭后 goroutine 与 SDK body 释放、注销/撤销后的订阅终止。运行 `go test ./internal/metrics ./internal/agent ./internal/httpapi ./internal/dockerops -count=1`。
- [ ] **Step 5: 真实采样验收。** 与同一容器 Docker stats 使用相同口径比对，不比较不同时间瞬时值；开/关日志页观察订阅释放，确认业务日志未写入控制端数据库。

## Task 3.2: 分钟聚合、24 小时历史与数据完整性

**Files:**
- Create: `internal/store/migrations/005_metrics.sql`, `internal/metrics/aggregate.go`, `internal/metrics/store.go`, `internal/metrics/ingest.go`。
- Create: `internal/httpapi/metrics.go`。
- Test: `internal/metrics/aggregate_test.go`, `internal/metrics/store_test.go`, `internal/metrics/ingest_test.go`。

**Interfaces:**
- Consumes: `metrics.Values`、`metrics.Derived` 与认证后的 node ID。
- Produces: `metrics.Point{NodeID, ContainerID string; At time.Time; Values Values; Valid bool}`。
- Produces: `metrics.Bucket{NodeID, ContainerID string; Start time.Time; Average, Peak Values; SampleCount int; ExpectedCount int}`。
- Produces: `metrics.NewAggregator() *Aggregator`；`(*Aggregator).Add(point Point)`；`FlushBefore(before time.Time) []Bucket`。
- Produces: `metrics.NewStore(db *sql.DB) *Store`；`(*Store).Save(ctx context.Context, buckets []Bucket) error`；`Query(ctx context.Context, nodeID, containerID string, from, to time.Time) ([]Bucket, error)`。

- [ ] **Step 1: 编写平均值、峰值和样本完整性测试。**

```go
func TestMinuteBucketRetainsPeakAndMissingSamples(t *testing.T) {
    agg := metrics.NewAggregator()
    start := time.Unix(1800000000, 0).UTC().Truncate(time.Minute)
    for i, cpu := range []float64{10, 90} {
        agg.Add(metrics.Point{NodeID: "n1", ContainerID: "c1", At: start.Add(time.Duration(i)*10*time.Second), Valid: true, Values: metrics.Values{CPUPercent: cpu}})
    }
    buckets := agg.FlushBefore(start.Add(time.Minute))
    if len(buckets) != 1 { t.Fatalf("buckets=%d", len(buckets)) }
    b := buckets[0]
    if b.Average.CPUPercent != 50 || b.Peak.CPUPercent != 90 || b.SampleCount != 2 || b.ExpectedCount != 6 {
        t.Fatalf("wrong aggregate: %+v", b)
    }
}
```

- [ ] **Step 2: 执行 RED。** `go test ./internal/metrics -run 'Bucket|History|Ingest' -count=1`。
- [ ] **Step 3: 实现聚合和数据库批量写入。** 以 node/container/minute 为唯一键，每项保留 sum/count/peak，避免错误地平均已有平均值。缺失整分钟不创建零行；查询缺口返回缺省点由前端映射 null。

```sql
CREATE TABLE metric_minutes (
    node_id TEXT NOT NULL,
    container_id TEXT NOT NULL,
    bucket_start INTEGER NOT NULL,
    sample_count INTEGER NOT NULL,
    expected_count INTEGER NOT NULL,
    average_json TEXT NOT NULL,
    peak_json TEXT NOT NULL,
    PRIMARY KEY (node_id, container_id, bucket_start)
);
```

样本时间超过接收时间前后 30 秒则标记时钟异常，不刷新实时值；容器 ID 必须存在于对应节点近期清单中。告警判断使用有效实时 Point，不使用分钟平均值。

数据库写批最大 100 条、上下文期限 500ms，聚合队列默认最多 6000 个桶；压力溢出丢弃最旧未写桶并累加可见缺失计数，不让指标请求无限等待部署写事务。所有队列参数可通过进程配置调整，不承诺支持无限规模。

历史 API 为 `GET /metrics`，必填查询参数 `node_id`、`container_id`、`from`、`to`（时间使用 UTC RFC3339）；只允许 24 小时窗口、分页与目标归属校验，返回 bucket complete 信息及单位；重放 bucket 幂等 upsert，不重复计数。API 不暴露其他节点的未经授权对象。

- [ ] **Step 4: 执行 GREEN。** 验证跨分钟、重复样本、错节点、未来/过期样本、容器重建、空窗口、压力丢弃、SQLite busy 不拖住部署、重启后历史可读。`go test ./internal/metrics ./internal/httpapi -count=1`。
- [ ] **Step 5: 真实历史验收。** 运行超过两分钟，核对实时与历史平均/峰值不同但正确；暂停 Agent 后曲线出现缺口而非零，重连后不伪造历史补齐。

## Task 3.3: 告警规则、持续状态、合并与恢复

**Files:**
- Create: `internal/store/migrations/006_alerts.sql`, `internal/alerts/rules.go`, `internal/alerts/evaluate.go`, `internal/alerts/events.go`, `internal/alerts/correlation.go`。
- Create: `internal/httpapi/alerts.go`。
- Modify: `internal/deploy/receipts.go`, `internal/agentlink/hub.go`。
- Test: `internal/alerts/evaluate_test.go`, `internal/alerts/events_test.go`, `internal/alerts/correlation_test.go`。

**Interfaces:**
- Produces: `alerts.Rule{ID, Kind, ScopeID, Metric string; Threshold float64; For time.Duration; Enabled bool}`；Kind 为 container_exit/agent_offline/resource。
- Produces: `alerts.Observation{ObjectID string; At time.Time; Value float64; Valid bool; ExpectedAction bool}`。
- Produces: `alerts.Event{ID, RuleID, ObjectID, State, Summary string; StartedAt time.Time; ResolvedAt *time.Time; AcknowledgedAt *time.Time}`。
- Produces: `alerts.New(db *sql.DB) *alerts.Engine`；`PutRule(ctx context.Context, rule Rule) error`；`Observe(ctx context.Context, ruleID string, value Observation) (*Event, error)`；`Acknowledge(ctx context.Context, eventID string, at time.Time) error`。
- Produces: `alerts.Notification{ID, EventID, State string; OccurredAt time.Time; Summary, Environment, Object string}`，由状态变化在同一事务生成，为 Task 3.4 消费。

- [ ] **Step 1: 编写缺失数据不恢复的失败测试。**

```go
func TestMissingSampleDoesNotResolveActiveAlert(t *testing.T) {
    db, err := store.Open(filepath.Join(t.TempDir(), "alerts.db"))
    if err != nil { t.Fatal(err) }
    defer db.Close()
    engine := alerts.New(db)
    ctx := context.Background()
    rule := alerts.Rule{ID: "r1", Kind: "resource", Metric: "cpu_percent", Threshold: 80, For: 0, Enabled: true}
    if err := engine.PutRule(ctx, rule); err != nil { t.Fatal(err) }
    at := time.Unix(1800000000, 0)
    opened, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at, Value: 90, Valid: true})
    if err != nil || opened == nil || opened.State != "active" { t.Fatalf("open: %v", err) }
    changed, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at.Add(10*time.Second), Valid: false})
    if err != nil || changed != nil { t.Fatalf("missing sample changed alert: %v", err) }
}
```

- [ ] **Step 2: 执行 RED。** `go test ./internal/alerts -count=1`。
- [ ] **Step 3: 实现持久状态机与事件去重。** 表包括 alert_rules、alert_conditions、alert_events、notification_outbox。`Observe` 只在新增/恢复等状态变化时返回 Event，持续活跃只更新 last_observed_at，不插入重复通知。

```sql
CREATE UNIQUE INDEX one_active_alert
ON alert_events(rule_id, object_id)
WHERE state = 'active';
CREATE UNIQUE INDEX one_notification_per_transition
ON notification_outbox(event_id, transition_seq);
```

连续条件由首次有效超限时间计算，采样缺失清除未触发的 pending 起点，不恢复已触发事件；连续恢复时间使用规则 For，避免阈值抖动反复发通知。超限采样间隔超过两倍默认采样间隔视为中断；重启保留已触发事件，但 pending 连续性需由新鲜数据重新证明。

失联由控制端的 last heartbeat 判断，不由 Agent 本机时间判断；60 秒触发后合并，重连产生恢复事件。控制端停机不“回补”假想通知。

容器退出关联具体应用任务和 Docker 事件：只抑制已确认替换的旧容器、明确 one-shot 成功或明确正常停止；新替代容器失败照常报警，不能应用级 blanket mute。平台外意图不明保留提示；`Acknowledge` 标记已确认，不伪造运行恢复。Swarm replicated 使用 service ID+slot，global 使用 service ID+node ID 关联，跨容器重建保留稳定对象身份。

API：`GET/POST /alert-rules`、`PUT /alert-rules/{id}`、`DELETE /alert-rules/{id}`、`GET /alerts`、`POST /alerts/{id}/acknowledge`；禁用/删除规则时保留历史且停止新通知。资源规则初始不启用虚构通用阈值。

- [ ] **Step 4: 执行 GREEN。** 覆盖 For 前后、缺失、重启、阈值抖动、并发重复采样只一事件、正常 one-shot、未知退出、部署中真正失败、Swarm 替换去重、失联与恢复各一次通知、确认不等于恢复。运行 `go test ./internal/alerts ./internal/httpapi -count=1`。
- [ ] **Step 5: 真实告警验收。** 对测试容器产生资源负载与意外退出，对测试 Agent 断线；分别验证持续阈值、事件合并与恢复，不影响生产容器。

## Task 3.4: 安全 Webhook 与有界持久重试

**Files:**
- Create: `internal/notify/policy.go`, `internal/notify/webhook.go`, `internal/notify/worker.go`, `internal/store/migrations/007_webhooks.sql`。
- Create: `internal/httpapi/webhooks.go`。
- Test: `internal/notify/policy_test.go`, `internal/notify/webhook_test.go`, `internal/notify/worker_test.go`。

**Interfaces:**
- Consumes: `alerts.Notification` 与事务生成的 notification_outbox。
- Produces: `notify.CheckAddress(addr netip.Addr, privateAllowlist []netip.Prefix) error`。
- Produces: `notify.WebhookConfig{ID, URL string; AllowedPrivateCIDRs []string; Enabled bool}`。
- Produces: `notify.NewSender(cfg WebhookConfig) (*notify.Sender, error)`；`(*Sender).Send(ctx context.Context, message alerts.Notification) error`。
- Produces: `notify.NewWorker(db *sql.DB, box *secret.Box, now func() time.Time) *notify.Worker`；`RunOnce(ctx context.Context) error`。
- Produces: `GET /webhook` 返回脱敏配置与通知状态，`PUT /webhook` 保存一个默认通用通知目标，`POST /webhook/test` 显式发送测试；测试/保存均需会话、同源与 CSRF 验证，列表不返回 URL 中的秘密。PUT 不提交 URL 表示保留原值，禁止把脱敏显示值写回覆盖原配置。

- [ ] **Step 1: 编写不可被内网白名单绕过的地址测试。**

```go
func TestWebhookNeverConnectsToLoopbackOrMetadata(t *testing.T) {
    allowed := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}
    for _, address := range []string{"127.0.0.1", "169.254.169.254", "::1", "::ffff:127.0.0.1"} {
        if err := notify.CheckAddress(netip.MustParseAddr(address), allowed); err == nil {
            t.Fatalf("unsafe address accepted: %s", address)
        }
    }
}
```

- [ ] **Step 2: 执行 RED。** `go test ./internal/notify -count=1`。
- [ ] **Step 3: 实现固定出站策略与有限重试。** 仅 http/https，拒绝 URL userinfo、fragment、超长地址；保存的 URL 可能包含凭据，因此加密保存、列表仅返回脱敏地址。私网 IP 必须管理员显式允许精确 CIDR；始终拒绝 loopback、link-local、unspecified、multicast、云元数据地址和控制端自身网卡地址。IPv4-mapped IPv6 先 Unmap 再检查。

解析 DNS 的所有结果并检查；连接阶段固定拨号至已检查 IP，TLS ServerName 保留原主机名，避免 DNS rebinding 和关闭证书验证。关闭默认环境代理和自动重定向，HTTP 客户端总超时 10 秒、响应体读取上限 64 KiB。

```go
client := &http.Client{
    Timeout: 10 * time.Second,
    CheckRedirect: func(req *http.Request, via []*http.Request) error {
        return errors.New("webhook redirects are not allowed")
    },
    Transport: transport,
}
```

`transport` 在 `NewSender` 构造：自定义 DialContext 实施以上 IP pinning，不能只是先检查 URL 后使用默认 Transport。

通知只有事件 ID、transition ID、环境、对象、状态、时间与摘要，不附日志/YAML/变量。接收方用 notification ID 去重；active 和 resolved 是不同 transition，不能只凭 event ID 把恢复去重掉。

每项先 durable claim，再发送，保存 HTTP 结果；失败最多 3 次额外重试，间隔 30 秒、2 分钟、10 分钟。重启恢复 pending/expired lease，网络结果未知允许重投，明确至少一次投递。队列最大 10000 个未终结通知，超过时保留站内事件并标记通知未入队，不无限增长；返回失败也不影响原部署事务。手动测试 Webhook 是外发动作，只在管理员点击时执行。

- [ ] **Step 4: 执行 GREEN。** 覆盖 loopback/metadata/IPv6、私网白名单、DNS 更换、redirect、禁用环境 proxy、超时、body 上限、500/429、4 次后终止、重启重复投递去重 ID、恢复通知、配置 URL 不泄漏。测试通过注入本地 RoundTripper/Resolver 而不是放宽生产 SSRF 策略。
- [ ] **Step 5: 外发验收。** 只向用户授权的测试接收端发送测试通知；核对 JSON、异常/恢复各一次，以及失败状态。没有接收地址授权时只完成隔离自动化测试，明确外发验收未执行。

## Task 3.5: 曲线、日志、告警页面与运行态反馈

**Files:**
- Create: `web/src/components/MetricChart.vue`, `web/src/components/LogViewer.vue`, `web/src/components/AlertRuleForm.vue`。
- Create: `web/src/views/AlertsView.vue`, `web/src/api/metrics.ts`, `web/src/api/alerts.ts`, `web/src/api/logs.ts`。
- Modify: `web/src/views/OverviewView.vue`, `web/src/views/ApplicationDetailView.vue`, `web/src/router.ts`, `web/src/components/AppShell.vue`, `web/src/api/types.ts`。
- Test: `web/src/components/MetricChart.test.ts`, `web/src/components/LogViewer.test.ts`, `web/src/components/AlertRuleForm.test.ts`, `web/e2e/observability.spec.ts`。

**Interfaces:**
- Consumes: 既有 SSE、metrics/alerts/rules/webhooks/log-subscriptions API。
- Produces: `MetricPoint = { at: string; average: number | null; peak: number | null; complete: boolean }`；`MetricChart` props 为 `points: MetricPoint[]`、`unit: string`。
- Produces: `LogViewer` props 为 `lines: string[]`、`truncated: boolean`，仅纯文本。
- Produces: `/alerts` 页面，主导航最终为概览/应用/告警/设置。

- [ ] **Step 1: 编写日志不执行 HTML 的失败测试。**

```ts
import { mount } from '@vue/test-utils'
import { expect, test } from 'vitest'
import LogViewer from './LogViewer.vue'

test('renders container output as text', () => {
  const payload = '<img src=x onerror=alert(1)>'
  const wrapper = mount(LogViewer, { props: { lines: [payload], truncated: false } })
  expect(wrapper.text()).toContain(payload)
  expect(wrapper.find('img').exists()).toBe(false)
})
```

- [ ] **Step 2: 执行 RED。** `npm --prefix web test -- LogViewer`；同时为 MetricChart 编写空点不转换成零、单位正确的测试。
- [ ] **Step 3: 实现准确的可视化状态。** 图表使用 ECharts 按需模块导入，平均和峰值两条线，禁止连过缺口：

```ts
const series = [
  { name: '平均值', type: 'line', connectNulls: false, data: props.points.map(p => [p.at, p.average]) },
  { name: '峰值', type: 'line', connectNulls: false, data: props.points.map(p => [p.at, p.peak]) },
]
```

历史 bucket 的缺失分钟明确填 null，只填时间坐标不伪造数值。最新数据过期显示“数据已过期”，节点失联显示“连接中断”；不写宿主机宕机。CPU 标注“每核 100%”，内存/网络/I/O 单位一致，资源曲线标为容器而非整机。

日志页按需打开/关闭订阅，缓冲上限 5000 行、超长行截断提示、暂停滚动和跟随不影响服务器任务；销毁页签取消订阅。禁止 v-html，渲染 `<pre>{{ text }}</pre>`。

告警页区分 active、resolved、acknowledged，规则输入显式阈值和持续时间，通知详情展示重试/失败但不泄密 URL。编辑 Webhook 的内网允许范围时给出安全说明；所有保存/确认/删除带 CSRF；错误保留用户输入。

- [ ] **Step 4: 执行 GREEN 和构建。** `npm --prefix web test`、`npm --prefix web run build`、`npm --prefix web run test:e2e -- observability`；覆盖空指标、长日志、掉线缺口、字段错误、保存失败、重复告警、恢复、通知失败和敏感内容遮罩。
- [ ] **Step 5: 浏览器实测。** 启动控制端与前端，真实 Agent 产生指标；切换页签观察网络订阅释放、内存不持续增长、无残余滚动/阴影裁切；桌面和窄屏截图，检查 console/network；不能只展示一张带假曲线的截图。

## Task 3.6: 数据保留、压力边界与完整验收

**Files:**
- Create: `internal/retention/prune.go`, `internal/retention/prune_test.go`。
- Create: `tests/integration/observability_test.go`, `tests/integration/acceptance_test.go`, `tests/integration/safety_test.go`。
- Modify: `cmd/control/main.go`, `internal/agent/ledger.go`, `web/e2e/observability.spec.ts`。

**Interfaces:**
- Produces: `retention.New(db *sql.DB) *retention.Cleaner`；`(*Cleaner).RunOnce(ctx context.Context, now time.Time) (retention.Counts, error)`；`Counts{Metrics, Deployments, Versions, Alerts, Audit, Notifications int}`。
- Consumes: 阶段一至三全部表和状态；Agent 清理自身已获终态确认且超过保留期的账本，未确认结果不清理。

- [ ] **Step 1: 编写过期指标清理测试。**

```go
func TestRetentionKeepsOnlyRecentMetrics(t *testing.T) {
    db, err := store.Open(filepath.Join(t.TempDir(), "retention.db"))
    if err != nil { t.Fatal(err) }
    defer db.Close()
    now := time.Unix(1800000000, 0).UTC()
    for _, at := range []time.Time{now.Add(-25*time.Hour), now.Add(-23*time.Hour)} {
        _, err := db.Exec(`INSERT INTO metric_minutes(node_id,container_id,bucket_start,sample_count,expected_count,average_json,peak_json) VALUES(?,?,?,?,?,?,?)`, "n1", "c1", at.UnixMilli(), 6, 6, "{}", "{}")
        if err != nil { t.Fatal(err) }
    }
    if _, err := retention.New(db).RunOnce(context.Background(), now); err != nil { t.Fatal(err) }
    var count int
    if err := db.QueryRow("SELECT COUNT(*) FROM metric_minutes").Scan(&count); err != nil { t.Fatal(err) }
    if count != 1 { t.Fatalf("remaining metrics=%d", count) }
}
```

- [ ] **Step 2: 执行 RED。** `go test ./internal/retention -count=1`。
- [ ] **Step 3: 实现按时间分批保留。** 指标 24 小时；已结束部署/已恢复告警/审计 90 天；活跃告警与 uncertain/running 等未结束任务不清理。每应用保留最近 50 版本，同时保留 latest、successful、未结束任务及尚保留的所有部署引用版本。删除顺序先过期输出/已结束关联通知与任务，再删不再被引用的历史版本；严格外键约束，不能启用级联把需要保护的数据删掉。

```sql
DELETE FROM metric_minutes
WHERE rowid IN (
    SELECT rowid FROM metric_minutes WHERE bucket_start < ? LIMIT 500
);
```

每批短事务、检查 context、分散定时，不在每次 HTTP 请求全量清理，不自动执行阻塞性的全库 VACUUM。通知 outbox 只删除终结且达到历史保留期的记录；仍待发送/结果不明保留。Agent 已完成账本需控制端确认接收终态后才能进入清理，且控制端不再下发已清理历史任务 ID。

- [ ] **Step 4: 执行 GREEN 并覆盖引用保护。** 构造超过 50 版本、最近成功版本在 50 之外、90 天前 uncertain 任务、仍被保留部署引用的旧版本、active 告警、未完成通知和大批指标，逐条断言不误删。`go test ./internal/retention ./internal/agent -count=1`。
- [ ] **Step 5: 执行完整自动化。** 在正确工具链下运行：

```bash
go test ./... -count=1
go vet ./...
go test -race ./...
npm --prefix web test
npm --prefix web run build
npm --prefix web run test:e2e
```

race 或浏览器运行条件不具备时单独记录未执行，不能用普通测试覆盖其结论。类型检查、构建通过不意味着 Docker 功能已验证。

- [ ] **Step 6: 执行已授权的真实验收。** integration tests 必须先检查 `HMOOB_TEST_ALLOW_MUTATION=1`、显式 Engine/节点清单、测试 namespace 前缀及实际环境身份，再创建资源。只读检查不需要 mutation 标志；缺配置时应明确 skip 原因，最终总结不能把 skip 算作通过。

```bash
go test -tags=integration ./tests/integration -count=1 -v
```

场景必须包含：Compose 更新、三节点 Swarm 滚动与回滚、多 Manager 正常 quorum 下新任务选择、执行中断线/重启/重复回执、服务删除确认、数据卷保护、历史重新部署、metric 缺口/保留、日志源释放、告警去重/恢复、Webhook 失败重试、未登录/CSRF/错误凭据/撤销/超大消息/命令注入边界。

- [ ] **Step 7: 检查容量和最终浏览器体验。** 增加采样/日志输入至队列上限，观察堆内存、goroutine、SQLite busy 与部署耗时；要求没有无界增长，丢样有明确缺口，告警/日志压力不阻塞任务回执。记录测试容器数与实际资源消耗，不凭空宣称性能上限。

从浏览器完成初始化→接入→创建/导入应用→保存部署→观察指标/日志→异常告警→恢复→历史重部署；再验证窄屏、空状态、长 YAML、任务失败、会话过期和网络中断，保存截图并检查控制台。

## 完整首版退出门槛

- [ ] 阶段一、二、三的测试与真实功能验收均有证据，未执行和 skipped 单独列出。
- [ ] 真实 Linux Compose、Manager+两个 Worker、正常 quorum 的多 Manager 场景完成；没有把 Docker Desktop 单节点模拟说成多节点验收。
- [ ] 站内告警与授权 Webhook 的异常/恢复投递得到验证；控制端停机与日志不集中保留的限制明确可见。
- [ ] 所有依赖锁定，构建可重复；生产控制端不挂 Docker socket，Agent 不暴露端口，TLS 验证开启。
- [ ] 不自动操作生产环境、不自动发布、不初始化 Git、不提交；需要这些动作时单独取得授权。
