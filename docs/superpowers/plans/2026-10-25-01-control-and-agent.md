# 阶段一：控制端与 Agent Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付可登录、可安全接入远端 Agent、可查看真实节点与容器清单的独立运行切片。

**Architecture:** 单实例 Go 控制端提供 API、Vue 静态资源和经过认证的 Agent WSS 入口。每个 Linux 节点运行一个 Agent，主动连接控制端，并从本机 Docker 获取清单；SQLite 保存身份与配置，不让浏览器直连 Docker。

**Tech Stack:** Go 1.26.10、标准库 HTTP、modernc.org/sqlite、golang.org/x/crypto、github.com/coder/websocket、Docker 官方 Go SDK、Vue 3、TypeScript 6.0.3、Vite 8、Vitest 4、Playwright；Node.js 25.8.1（现有环境）。

**Spec:** [已批准设计](../specs/2026-10-24-hmoob-dock-design.md)。本文为整个实施工作的入口。

## Global Constraints

以下约束同样适用于后续两个阶段：

- 单管理员，不引入多用户、角色权限或多租户。
- Go 控制端、Go Agent、Vue 3 + TypeScript 前端、SQLite。
- 不要求外部数据库、Redis、消息队列、Prometheus 或 Alertmanager。
- 单机 Docker Compose v2、多节点 Swarm Stack；控制端独立部署，每节点一个 Agent。
- Agent 主动通过经过证书校验的 WSS 长连接接入控制端。
- 同一应用同时只能有一个未结束的部署任务；状态待确认仍占用互斥。
- 不自动更新控制端和 Agent 自身，不创建 Swarm，不自动安装 Docker。
- CPU 按每核 100% 的口径展示，多核使用可超过 100%；内存采用 Docker API 上报的当前使用字节数。
- 指标采集间隔：10 秒；历史聚合粒度与保留：1 分钟，24 小时。
- Agent 心跳与失联判断：10 秒心跳，持续 60 秒未收到有效心跳判定失联。
- 一次性接入令牌有效期：15 分钟，成功使用一次即失效。
- 单任务已保存输出上限：2 MiB，超限截断并提示。
- 已结束部署、告警和审计记录：默认保留 90 天。
- 配置版本：每应用保留最近 50 个；正在执行和最近成功版本不因数量上限删除。保留的部署记录引用也必须保护。
- Webhook 重试：初次失败后最多重试 3 次，采用退避间隔。
- 资源告警阈值：由管理员显式配置，不凭空采用统一生产阈值。
- 部署执行与就绪等待：分别有可配置期限，默认各 10 分钟；超时不等于 Docker 操作已停止。
- 不提供任意远程 Shell，不默认删除卷，不对未知结果盲目重试，不将 CLI 成功等同于服务健康。
- 所有数据库行为使用临时真实 SQLite 测试；真实 Docker 和浏览器验收不能被 Mock 通过替代。
- 当前不是 Git 仓库。未获另行授权前，不初始化 Git、不提交、不推送；每任务用测试结果和文件变更检查作检查点。

---

## 0. 分阶段交付与执行前提

| 阶段 | 独立可用成果 | 依赖 |
| --- | --- | --- |
| 本文：控制端与 Agent | 登录、接入/撤销、节点和容器清单、容器化运行 | 无 |
| [阶段二：应用部署](2026-10-25-02-deployment.md) | Compose/Swarm YAML 部署、执行结果、断线核对、历史重部署 | 阶段一 |
| [阶段三：监控告警](2026-10-25-03-observability.md) | 实时/历史指标、按需日志、规则与 Webhook、完整验收 | 阶段一与阶段二 |

三个阶段都通过后才满足首版完整范围。以下任务是设计内的实现顺序，不是削减已经确认的能力。

### 开发环境证据与处理

规划时实际检查：项目只有设计文档；终端 PATH 找不到 `go`；Docker CLI 为 29.3.0，但 Docker Desktop Linux Engine 管道不存在；PATH 中 Node 为 25.8.1，npm 为 11.11.0。

官方 Go 下载接口当时列出稳定版 1.27.3 和 1.26.10；选择 1.26.10 作为一致工具链。npm 当时返回 Vue 3.5.44、Vite 8.3.5、TypeScript 7.0.2、Vitest 5.0.5。Vitest 5.0.5 的 Node 范围是 `^22.12.0 || ^24.0.0 || >=26.0.0`，不包含现有 Node 25；Vitest 4.x 的范围是 `^20.0.0 || ^22.0.0 || >=24.0.0`，覆盖 Node 25.8.1。因此固定使用 Vitest 4.x 与 Vite 8，不采用 Vitest 5，也不为此单独安装另一个 Node 版本。TypeScript 固定 6.0.3 而非 7.0.2：`vue-tsc@3.3.13` 依赖 `typescript/lib/tsc` 子路径，TypeScript 7 已移除该导出，且不存在支持 TypeScript 7 的更新版 vue-tsc，而 `vue-tsc --noEmit` 是固定的构建命令。

执行前需要授权安装 Go 1.26.10、初始化本地 git 仓库（仅本地提交，不推送）并启动 Docker 引擎用于真实环境验证。不得在没有授权的情况下改动系统工具、替换 PATH、切换用户 Docker context 或启动桌面服务。单元测试不要求生产服务器；Swarm 验收另需明确授权的隔离 Linux 节点，若仅能提供本机 Docker Desktop，则三节点 Swarm 与多 Manager quorum 场景记为未验收。

依赖只在开始编码后安装，锁入 `go.mod`/`go.sum` 与 `web/package-lock.json`；以无强制覆盖的安装和构建验证兼容性。不使用 `--force`、`--legacy-peer-deps` 掩盖不兼容。Docker SDK 选择与目标 Engine/API 兼容的官方版本，启用 API negotiation；在 `go.mod` 中固定选定版本，不能在不同任务中切换 SDK 包族。

### 目录职责

| 文件/目录 | 责任 |
| --- | --- |
| `cmd/control/main.go` | 控制端配置、数据库、后台服务和 HTTP 装配 |
| `cmd/agent/main.go` | Agent 配置、Docker 连接、持久化身份和连接循环 |
| `internal/config/config.go` | 配置加载与安全默认值 |
| `internal/store/db.go`、`internal/store/migrations/*.sql` | SQLite 连接、迁移和基础事务 |
| `internal/secret/box.go` | AES-GCM 加密与上下文绑定 |
| `internal/auth/` | 初始化、密码、会话、CSRF 与登录节流 |
| `internal/agentlink/` | 注册、凭据、WSS 会话、身份绑定和消息分发 |
| `internal/protocol/` | 有版本的消息封装和各阶段消息类型 |
| `internal/dockerops/inventory.go` | Docker SDK 到平台清单数据的转换 |
| `internal/agent/runtime.go` | Agent 连接、心跳、清单、重连 |
| `internal/httpapi/` | 管理 API、认证挂接、静态资源及统一错误格式 |
| `web/src/` | Vue 页面、API 客户端、路由、通用纯文本组件 |
| `deploy/` | 控制端与 Agent 镜像构建和单机运行样例 |
| `tests/integration/`、`web/e2e/` | 真实数据库、协议、Docker 与浏览器验证 |

所有路径为计划中的新增文件，不表示它们已经存在。代码块固定关键接口、测试和算法；执行者必须实现各清单中的完整行为，不将示例测试通过当作整个任务完成。

### 公共约定

- Go module 为 `hmoob-dock`，按功能包分割，避免单个 `server.go` 包含全部业务。
- JSON 字段统一 `snake_case`；时间为 UTC RFC3339Nano；外部 ID 使用加密随机标识，禁止用户控制文件路径。
- 计划接口中 `{ID, Name string}` 等是字段契约的紧凑写法，落入协议/API 的类型必须逐字段添加 JSON tag，并以序列化契约测试验证；时长字段明确单位，不能由前后端猜测。
- API 错误为 `{"code":"version_conflict","message":"配置已更新，请重新加载"}`，校验错误可附 `fields`；日志不记录请求正文。
- API 均在 `/api/v1`；浏览器实时数据使用同源、Cookie 认证的 SSE；Agent 使用 WSS。浏览器不在 URL 携带令牌。
- HTTP 测试可用 `httptest`；生产只接受 HTTPS 或显式信任的本机反向代理，不信任任意来源的 `X-Forwarded-*`。
- 每任务结束运行其指定测试；所有纯 Go 任务运行 `go test ./...`，支持 race 的工具链再运行 `go test -race ./...`。不能因 Windows 缺 C 编译器而把 race 测试称为已通过。

## Task 1.1: 可启动的控制端、真实 SQLite 与配置加密

**Files:**
- Create: `go.mod`, `go.sum`, `cmd/control/main.go`, `internal/config/config.go`。
- Create: `internal/store/db.go`, `internal/store/migrations/001_core.sql`, `internal/secret/box.go`, `internal/httpapi/server.go`。
- Test: `internal/store/db_test.go`, `internal/secret/box_test.go`, `internal/httpapi/server_test.go`。

**Interfaces:**
- Produces: `store.Open(path string) (*sql.DB, error)`，打开后完成迁移，启用 foreign keys、WAL 和有限 busy timeout。
- Produces: `secret.New(key []byte) (*secret.Box, error)`；`(*Box).Seal(plain, aad []byte) ([]byte, error)`；`(*Box).Open(ciphertext, aad []byte) ([]byte, error)`。
- Produces: `httpapi.NewServer(db *sql.DB, box *secret.Box, cfg config.Control) (http.Handler, error)`；后续任务扩展其内部装配，不改变构造入口。
- Produces: `config.Control{DBPath, KeyFile, BootstrapFile, PublicURL, TLSCert, TLSKey, WebDir string}`。启动校验主密钥不在数据库目录内。

- [ ] **Step 1: 为加密上下文隔离编写失败测试。**

```go
func TestCiphertextIsBoundToVersion(t *testing.T) {
    box, err := secret.New(bytes.Repeat([]byte{7}, 32))
    if err != nil { t.Fatal(err) }
    ciphertext, err := box.Seal([]byte("PASSWORD=secret"), []byte("app-1/version-1"))
    if err != nil { t.Fatal(err) }
    if _, err := box.Open(ciphertext, []byte("app-2/version-1")); err == nil {
        t.Fatal("cross-application ciphertext accepted")
    }
}
```

同轮在 `db_test.go` 用 `t.TempDir()` 创建文件数据库、写入迁移版本、关闭重开，再验证外键生效及数据保留。测试文件使用所列公开接口和标准库导入，不使用全局共享数据库。

- [ ] **Step 2: 执行 RED。** 先建立最小 module 和测试导入，再运行 `go test ./internal/store ./internal/secret ./internal/httpapi -count=1`。应因缺少目标接口或缺失迁移行为失败，而不是因为工具链缺失；工具链缺失属于阻塞。
- [ ] **Step 3: 实现加密、迁移和健康端点。** AES-GCM 使用独立随机 nonce，AAD 绑定应用/版本，拒绝短密文和错误密钥；核心实现必须保留以下顺序：

```go
func (b *Box) Seal(plain, aad []byte) ([]byte, error) {
    nonce := make([]byte, b.aead.NonceSize())
    if _, err := rand.Read(nonce); err != nil { return nil, err }
    return b.aead.Seal(nonce, nonce, plain, aad), nil
}
```

`001_core.sql` 创建 `schema_migrations(version INTEGER PRIMARY KEY)` 和 `audit_events(id TEXT PRIMARY KEY, occurred_at INTEGER NOT NULL, action TEXT NOT NULL, target_id TEXT NOT NULL, result TEXT NOT NULL)`。所有时间存 UTC 毫秒。迁移事务失败时启动失败，不忽略迁移错误。写连接序列化，短只读连接池单独处理查询。

`GET /healthz` 仅返回存活信息，不暴露配置或数据库路径。主密钥为 32 字节随机文件，启动不自动更换现有密钥；缺失或格式错误明确停止。初始化凭据尚未被消费时，不在启动日志打印其内容。

- [ ] **Step 4: 执行 GREEN。** 运行 Step 2 命令，补充错密钥、损坏密文、迁移回滚、启动缺密钥和健康检查测试，然后运行 `go test ./...`。
- [ ] **Step 5: 检查交付。** 启动控制端访问 `/healthz`；查看数据库实际位于持久化路径，日志不包含测试明文。记录通过的命令；不提交 Git。

## Task 1.2: 单管理员初始化、登录和会话安全

**Files:**
- Create: `internal/auth/service.go`, `internal/auth/password.go`, `internal/auth/middleware.go`, `internal/auth/ratelimit.go`。
- Create: `internal/store/migrations/002_auth.sql`, `internal/httpapi/auth.go`。
- Modify: `internal/httpapi/server.go`。
- Test: `internal/auth/service_test.go`, `internal/httpapi/auth_test.go`。

**Interfaces:**
- Consumes: `store.Open`、`config.Control`、`httpapi.NewServer`。
- Produces: `auth.New(db *sql.DB, now func() time.Time) *auth.Service`。
- Produces: `(*Service).SeedBootstrap(ctx context.Context, token string, expires time.Time) error`；`Initialize(ctx context.Context, token, password string) error`；`Login(ctx context.Context, password string) (auth.Session, error)`。
- Produces: `auth.Session{ID, Token, CSRF string; ExpiresAt time.Time}`；`Authenticate(ctx context.Context, token string) (auth.Session, error)`；`ChangePassword(ctx context.Context, token, current, next string) error`；`Logout(ctx context.Context, token string) error`。

- [ ] **Step 1: 为抢先初始化保护编写失败测试。**

```go
func TestInitializeConsumesBootstrapOnce(t *testing.T) {
    db, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
    if err != nil { t.Fatal(err) }
    defer db.Close()
    now := time.Unix(1800000000, 0)
    svc := auth.New(db, func() time.Time { return now })
    ctx := context.Background()
    if err := svc.SeedBootstrap(ctx, "deployment-secret", now.Add(15*time.Minute)); err != nil { t.Fatal(err) }
    if err := svc.Initialize(ctx, "wrong-secret", "a-strong-test-password"); err == nil { t.Fatal("unguarded initialization") }
    if err := svc.Initialize(ctx, "deployment-secret", "a-strong-test-password"); err != nil { t.Fatal(err) }
    if err := svc.Initialize(ctx, "deployment-secret", "replacement-password"); err == nil { t.Fatal("bootstrap reused") }
}
```

- [ ] **Step 2: 执行 RED。** `go test ./internal/auth ./internal/httpapi -run 'Initialize|Auth|Session' -count=1`，确认初始化保护测试失败。
- [ ] **Step 3: 实现持久化身份和 Cookie。** 创建单行 `administrator`（主键检查只允许 1）、`bootstrap_tokens`、`sessions` 表。密码使用 Argon2id 编码保存 salt/参数/hash，参数从服务自己的记录解析并限制范围；随机令牌只存 SHA-256 校验值。令牌比较采用常量时间比较。

```go
cookie := &http.Cookie{
    Name: "hmoob_session", Value: session.Token, Path: "/",
    HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
    Expires: session.ExpiresAt,
}
http.SetCookie(w, cookie)
```

API：`POST /auth/initialize`、`POST /auth/login`、`GET /auth/session`、`POST /auth/logout`、`POST /auth/password`。登录/初始化要求同源 Origin；已登录修改请求还必须校验 `X-CSRF-Token`。`/auth/session` 返回 CSRF 和到期时间，不返回会话 token。密码长度 12–128 字符，输入正文 8 KiB 上限；登录按来源限制尝试且使用通用失败文案；改密事务撤销所有会话。不得把代理伪造的 IP 当作节流来源。

- [ ] **Step 4: 执行 GREEN 并扩展真实 HTTP 测试。** 验证错误令牌/超时令牌/并发初始化、错误密码、限流、跨 Origin、缺 CSRF、Cookie 属性、改密后旧会话失效、日志无密码。运行 `go test ./internal/auth ./internal/httpapi -count=1`。
- [ ] **Step 5: 检查交付。** 用 `httptest.NewTLSServer` 完成初始化→登录→查询会话→改密→旧会话被拒绝全流程，运行 `go test ./...`。

## Task 1.3: Agent 安全接入、WSS 心跳与真实 Docker 清单

**Files:**
- Create: `cmd/agent/main.go`, `internal/protocol/envelope.go`, `internal/protocol/inventory.go`。
- Create: `internal/agentlink/enrollment.go`, `internal/agentlink/hub.go`, `internal/agent/runtime.go`, `internal/agent/identity.go`。
- Create: `internal/dockerops/inventory.go`, `internal/store/migrations/003_agents.sql`, `internal/httpapi/agents.go`。
- Modify: `internal/config/config.go`, `internal/httpapi/server.go`。
- Test: `internal/agentlink/enrollment_test.go`, `internal/agentlink/hub_test.go`, `internal/agent/runtime_test.go`, `tests/integration/agent_inventory_test.go`。

**Interfaces:**
- Produces: `protocol.Envelope{Version int; Type, RequestID string; Seq uint64; Payload json.RawMessage}`，wire version 为 1。
- Produces: `protocol.NodeIdentity{EngineID, SwarmID, SwarmNodeID, Role, Hostname string}`；`protocol.Container{ID, Name, Image, State, Health string; Labels map[string]string}`。
- Produces: `protocol.Inventory{Identity NodeIdentity; Containers []Container; CollectedAt time.Time}`。
- Produces: `agentlink.NewEnrollment(db *sql.DB, now func() time.Time) *agentlink.Enrollment`；`Issue(ctx context.Context) (string, error)`；`Redeem(ctx context.Context, token string, identity protocol.NodeIdentity) (agentlink.Credential, error)`；`Revoke(ctx context.Context, nodeID string) error`；`Credential{NodeID, Token string}`。
- Produces: `agentlink.NewHub(db *sql.DB) *agentlink.Hub`；`(*Hub).Send(ctx context.Context, nodeID string, message protocol.Envelope) error`；`Connected(nodeID string) bool`；`Subscribe(kind string, fn func(context.Context, string, protocol.Envelope) error)`。
- Produces: `(*Hub).ReadInventory(ctx context.Context, mode, targetID string) (protocol.Inventory, error)`；Compose 读取目标节点，Swarm 读取目标集群已认证 Manager 的清单，断线或过期返回错误，禁止用旧缓存完成部署预检。
- Produces: `dockerops.NewLocal(socketPath string) (*dockerops.Local, error)`；`(*Local).Inventory(ctx context.Context) (protocol.Inventory, error)`。
- Produces: `agent.Run(ctx context.Context, cfg config.Agent) error`，`config.Agent{ServerURL, CAFile, CredentialFile, EnrollmentFile, DockerSocket, WorkRoot string}`。

- [ ] **Step 1: 编写一次性接入测试。**

```go
func TestEnrollmentTokenCannotRegisterTwoNodes(t *testing.T) {
    db, err := store.Open(filepath.Join(t.TempDir(), "nodes.db"))
    if err != nil { t.Fatal(err) }
    defer db.Close()
    svc := agentlink.NewEnrollment(db, time.Now)
    ctx := context.Background()
    token, err := svc.Issue(ctx)
    if err != nil { t.Fatal(err) }
    first, err := svc.Redeem(ctx, token, protocol.NodeIdentity{EngineID: "engine-a"})
    if err != nil || first.NodeID == "" { t.Fatalf("first enrollment: %v", err) }
    if _, err := svc.Redeem(ctx, token, protocol.NodeIdentity{EngineID: "engine-b"}); err == nil { t.Fatal("token replay accepted") }
}
```

- [ ] **Step 2: 执行 RED。** `go test ./internal/agentlink ./internal/agent -count=1`。
- [ ] **Step 3: 实现注册和认证后的有界连接。** 管理 API 为 `POST /agent-tokens`、`GET /nodes`、`POST /nodes/{id}/revoke`；机器入口为 `POST /agent/enroll` 和 `GET /agent/connect`。机器入口不使用管理员 Cookie，分别验证短期注册令牌和独立节点凭据。

```go
if env.Version != 1 || len(env.Payload) > 1024*1024 {
    return errors.New("unsupported or oversized agent message")
}
if env.Type != "hello" && env.Type != "heartbeat" && env.Type != "inventory" {
    return errors.New("unsupported message type")
}
```

当前阶段只接受这三种类型；后续阶段显式扩展类型注册。每连接单写循环，控制帧和监控帧分开有界缓冲，慢连接不得阻塞全部节点。超过 1 MiB 的清单按批发送，每批有快照 ID/批号/结束标志，不截断后冒充完整清单。

注册事务消费令牌并创建节点；相同 EngineID 不自动替换已有节点。凭据保存到 0600 原子替换文件，不能放 argv 或 URL。断线按带抖动的指数退避重连，最高 30 秒；校验证书和主机名，不添加 `InsecureSkipVerify`。10 秒心跳与清单刷新，失联以控制端收到心跳的时间判断。

Docker SDK 读取 Info、容器列表、必要的 inspect；仅 Manager 请求 Swarm 节点/服务/任务。Agent 用自己的凭据绑定上报来源，不采信 payload 自报的控制端 node ID。

- [ ] **Step 4: 执行 GREEN。** 覆盖过期令牌、并发兑换、撤销后断开、错误证书、错主机名、伪造 EngineID、超大消息、重复连接替换、断线重连和空 Docker 环境。`go test ./internal/agentlink ./internal/agent ./internal/dockerops -count=1`。
- [ ] **Step 5: 真实清单验收。** 在获授权的 Linux Docker 环境运行带 `integration` tag 的 `TestAgentInventory`，只读取容器，验证平台数量、身份和状态与 Docker 一致。没有测试引擎时标记集成验收阻塞，不用模拟清单替代。

## Task 1.4: 登录、概览和 Agent 设置的可用前端

**Files:**
- Create: `web/package.json`, `web/package-lock.json`, `web/index.html`, `web/vite.config.ts`, `web/tsconfig.json`。
- Create: `web/src/main.ts`, `web/src/App.vue`, `web/src/router.ts`, `web/src/styles.css`。
- Create: `web/src/api/client.ts`, `web/src/api/types.ts`, `web/src/auth/session.ts`。
- Create: `web/src/views/InitializeView.vue`, `web/src/views/LoginView.vue`, `web/src/views/OverviewView.vue`, `web/src/views/SettingsView.vue`。
- Create: `web/src/components/AppShell.vue`, `web/src/components/NodeTable.vue`。
- Test: `web/src/components/NodeTable.test.ts`, `web/src/auth/session.test.ts`, `web/e2e/auth-and-nodes.spec.ts`, `web/playwright.config.ts`。

**Interfaces:**
- Consumes: Task 1.2 和 1.3 的 HTTP API。
- Produces: `api<T>(path: string, init?: RequestInit): Promise<T>`，处理同源 Cookie、CSRF、结构化错误，401 清理会话。
- Produces: `NodeSummary = { id: string; hostname: string; connected: boolean; container_count: number; last_seen: string | null }`。
- Produces: `NodeTable` 接受 `nodes: NodeSummary[]`；路由 `/initialize`、`/login`、`/`、`/settings`。

- [ ] **Step 1: 为断线状态编写失败组件测试。**

```ts
import { mount } from '@vue/test-utils'
import { expect, test } from 'vitest'
import NodeTable from './NodeTable.vue'

test('reports a disconnected agent without claiming the host is down', () => {
  const wrapper = mount(NodeTable, { props: { nodes: [{
    id: 'node-1', hostname: 'worker-1', connected: false,
    container_count: 3, last_seen: null,
  }] } })
  expect(wrapper.text()).toContain('连接中断')
  expect(wrapper.text()).not.toContain('服务器宕机')
})
```

- [ ] **Step 2: 建立测试配置并执行 RED。** 包 scripts 固定为 `dev`、`build`（`vue-tsc --noEmit && vite build`）、`test`（`vitest run`）、`test:e2e`（`playwright test`）。运行 `npm --prefix web test -- NodeTable`，确认因目标行为未实现而失败。
- [ ] **Step 3: 实现列表优先的界面。** 使用 Vue Router、原生语义 HTML 和统一间距/颜色变量；不引入大体积后台模板。节点状态从 API 获取，只有节点真实清单才显示数量。核心呈现：

```vue
<tr v-for="node in nodes" :key="node.id">
  <th scope="row">{{ node.hostname }}</th>
  <td>{{ node.connected ? '已连接' : '连接中断' }}</td>
  <td>{{ node.container_count }}</td>
</tr>
```

登录成功跳转概览；初始化成功进入登录；改密后返回登录。设置里可以生成短期接入令牌、查看已接入节点和确认撤销；令牌只在成功响应后短时展示，不存 localStorage。接入命令引用凭据文件，不把可复用令牌拼入 Shell 历史。应用和告警入口在对应阶段可用前不提供假数据或无效按钮。

- [ ] **Step 4: 执行 GREEN 和构建。** `npm --prefix web test`、`npm --prefix web run build`。覆盖键盘操作、输入错误、401、空节点列表、长主机名、撤销取消及失败重试。
- [ ] **Step 5: 启动服务并在浏览器操作。** 使用真实控制端，Vite 同源代理 `/api` 到控制端并在获允许的开发证书下运行 HTTPS；验证初始化/登录/改密/撤销、320px 和桌面宽度、控制台无异常并截图。开发证书不加入系统信任库，除非用户另行同意。

## Task 1.5: 控制端与 Agent 容器交付

**Files:**
- Create: `deploy/Dockerfile.control`, `deploy/Dockerfile.agent`, `deploy/compose.control.yaml`, `deploy/compose.agent.yaml`, `.dockerignore`。
- Create: `internal/httpapi/static.go`, `internal/httpapi/static_test.go`, `tests/integration/packaging_test.go`。
- Modify: `cmd/control/main.go`, `internal/config/config.go`。

**Interfaces:**
- Consumes: 两个可构建入口、`web/dist`、已实现的配置字段。
- Produces: Go 二进制 `control` 和 `agent`，控制端镜像内 `/app/web` 静态文件目录。
- Produces: 控制端通过 `HMOOB_DB_PATH`、`HMOOB_KEY_FILE`、`HMOOB_BOOTSTRAP_FILE`、`HMOOB_PUBLIC_URL`、`HMOOB_TLS_CERT`、`HMOOB_TLS_KEY`、`HMOOB_WEB_DIR` 配置；Agent 对应 `HMOOB_SERVER_URL`、`HMOOB_CA_FILE`、`HMOOB_CREDENTIAL_FILE`、`HMOOB_ENROLLMENT_FILE`、`HMOOB_DOCKER_SOCKET`、`HMOOB_WORK_ROOT`。

- [ ] **Step 1: 编写 API 不落入 SPA fallback 的失败测试。**

```go
func TestMissingAPIRouteIsNotHTML(t *testing.T) {
    dir := t.TempDir()
    if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<main>app</main>"), 0600); err != nil { t.Fatal(err) }
    handler := httpapi.StaticHandler(dir)
    w := httptest.NewRecorder()
    handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/missing", nil))
    if w.Code != http.StatusNotFound { t.Fatalf("status=%d", w.Code) }
}
```

本任务定义 `httpapi.StaticHandler(dir string) http.Handler`，拒绝 API 路径和路径穿越，仅对前端导航返回 index。

- [ ] **Step 2: 执行 RED。** `go test ./internal/httpapi -run 'Static|MissingAPI' -count=1`。
- [ ] **Step 3: 编写多阶段构建与受限运行样例。** 控制端构建阶段运行前端 build 和 Go build，运行阶段不需要 Node、Docker CLI 或 Docker socket。Agent 镜像带 Docker CLI、Compose v2 插件、CA 证书及 Agent 二进制；必须在构建中运行 `docker --version`、`docker compose version`，锁定已验证基础镜像 digest，不写 `latest`。

```yaml
services:
  agent:
    image: hmoob-dock-agent:local
    restart: unless-stopped
    environment:
      HMOOB_WORK_ROOT: /opt/hmoob-dock
      HMOOB_DOCKER_SOCKET: /var/run/docker.sock
      HMOOB_CREDENTIAL_FILE: /opt/hmoob-dock/agent-credential
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /opt/hmoob-dock:/opt/hmoob-dock
```

完整样例补入服务器 URL、CA 和接入令牌文件的必填变量与只读挂载，凭据不写入仓库。控制端仅暴露 HTTPS 服务端口，数据库卷与密钥文件分开挂载；Agent 不映射端口、不使用 `privileged`、不挂载整个宿主机根目录。Compose bind 路径两侧必须相同。

- [ ] **Step 4: 执行 GREEN 并构建镜像。** `go test ./...`、`npm --prefix web run build`；Docker 可用且用户允许构建后运行 `docker build -f deploy/Dockerfile.control -t hmoob-dock-control:local .` 和 Agent 对应命令。Compose 样例使用必填变量，没有变量时 `docker compose config` 必须失败而不是生成不安全默认凭据。
- [ ] **Step 5: 独立阶段验收。** 在测试 Linux 主机启动两个容器，浏览器完成登录和节点查看；重启控制端后管理员与节点仍存在；撤销 Agent 后不能重连，业务容器不受影响。运行 `go test -tags=integration ./tests/integration -run 'AgentInventory|Packaging' -count=1`；缺环境则明确未验收。

## 阶段一退出门槛

- [ ] 五个任务的自动化测试通过，凭据不在输出、数据库明文或命令历史出现。
- [ ] 真正启动开发服务，浏览器 golden path、错误状态与窄屏验证完成。
- [ ] Linux 节点与 Docker 容器清单来自真实 Agent；容器打包和重启持久化验证完成。
- [ ] 未验证项、工具链限制与用户可见行为记录在执行结果中；不把该阶段称作完整平台。

## 全设计覆盖索引

| 设计章节 | 实施任务 |
| --- | --- |
| 1–3 产品与架构 | 1.1、1.3、1.5；2.6 |
| 4 YAML、应用、历史版本 | 2.1、2.2、2.4、2.5、2.7 |
| 5 可靠性与错误 | 2.2、2.3、2.6、2.7 |
| 6 监控与日志 | 3.1、3.2、3.5 |
| 7 告警 | 3.3、3.4、3.5 |
| 8 安全 | 1.1–1.3、2.1、2.4、3.4 |
| 9 数据与保留 | 1.1、2.2、3.2、3.3、3.6 |
| 10 页面 | 1.4、2.7、3.5 |
| 11 验收 | 每任务 RED/GREEN，1.5、2.7、3.6 真实环境门槛 |

执行方式由用户选择：逐任务子代理实现并审查，或当前会话按阶段分批执行。计划本身不启动服务、不安装工具、不改系统权限，也不授权操作生产环境。
