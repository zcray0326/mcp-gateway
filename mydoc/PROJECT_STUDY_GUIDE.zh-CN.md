# MCP Gateway 快速阅读与面试准备指南

> 目标：用较短时间理解 MCP Gateway 的核心设计，能够解释简历中的四条项目描述，并能顺着代码讲清一次 MCP Server 注册和一次 Tool 调用。

## 1. 先用一句话理解项目

MCP Gateway 是一个自托管 MCP 网关：AI 客户端只连接 MCP Gateway，MCP Gateway 再统一连接多个上游 MCP Server，并负责能力聚合、名称路由、连接管理、访问控制和指标采集。

它最关键的设计是同时扮演两个角色：

```text
Claude / Cursor / Codex
        │
        │ MCP 请求
        ▼
MCP Gateway
  对下游：MCP Server
  对上游：MCP Client
        │
        ├── Context7 MCP Server
        ├── GitHub MCP Server
        └── Filesystem MCP Server
```

理解这两个角色以后，项目的代码就可以拆成两条主线：

1. **注册链路**：连接上游服务器，发现 Tools、Prompts、Resources，将元数据保存到数据库并注册到网关。
2. **调用链路**：接收下游请求，根据名称找到上游服务器，检查权限，获取连接并转发请求。

---

## 2. 阅读前必须掌握的 MCP 概念

### 2.1 MCP 解决什么问题

大模型本身只能生成内容，不能直接读取本地文件、查询数据库或操作 GitHub。MCP（Model Context Protocol）定义了一套通用协议，让 AI 应用可以发现并使用外部能力。

如果没有 MCP，不同 AI 应用和外部系统之间容易形成大量专用集成：

```text
AI 应用 × 外部系统 = 大量重复适配
```

使用 MCP 后，AI 应用只需要实现 MCP Client，外部能力只需要实现 MCP Server。

### 2.2 Host、Client、Server

- **MCP Host**：Claude Desktop、Cursor、Codex 等承载用户交互的 AI 应用。
- **MCP Client**：Host 内负责与某个 MCP Server 建立连接并发送协议请求的组件。
- **MCP Server**：向客户端公开 Tools、Prompts、Resources 等能力的程序。

日常讨论中常把 Host 也简称为“客户端”。在面试中最好知道严格角色，但不必刻意纠正所有口语表达。

MCP Gateway 的特殊之处是：它对 Claude 等下游表现为 MCP Server，对 Context7 等上游表现为 MCP Client。

### 2.3 Tools、Resources、Prompts

#### Tool

Tool 是可执行能力，例如：

- `search_code(query)`
- `create_issue(title, body)`
- `read_file(path)`

Tool 通常包含名称、描述和 JSON Schema 格式的输入参数定义。AI 模型根据描述和 Schema 决定如何调用，服务端执行后返回文本、图片或结构化内容。

项目中的公开类型见 `pkg/types/mcp_tool.go`，数据库模型见 `internal/model/mcp_tool.go`。

#### Resource

Resource 是可以读取的上下文数据，用 URI 标识，例如文档、文件或数据库记录。它更接近“读取数据”，而 Tool 更接近“执行动作”。

项目需要重写 Resource URI，避免多个上游使用相同 URI。核心代码在 `internal/service/mcp/resource.go`。

#### Prompt

Prompt 是服务器提供的可复用提示词模板，可以声明参数，渲染后返回一组用户或助手消息。

项目中的处理代码在 `internal/service/mcp/prompt.go`。

### 2.4 MCP 的典型生命周期

MCP 消息建立在 JSON-RPC 语义之上。一条连接通常经历：

```text
建立传输连接
    ↓
initialize：交换协议版本、客户端信息和 capabilities
    ↓
notifications/initialized：初始化完成
    ↓
tools/list、prompts/list、resources/list：发现能力
    ↓
tools/call、prompts/get、resources/read：使用能力
```

`capabilities` 很重要：客户端不应假设所有服务器都支持 Tools、Prompts 和 Resources，而应根据初始化结果判断。

在 MCP Gateway 注册上游服务器时，会先完成连接和初始化，再根据服务器 capabilities 拉取对应能力。

### 2.5 三种 Transport

Transport 负责承载 MCP 协议消息，不改变 Tool、Prompt、Resource 的语义。

#### STDIO

网关启动一个本地子进程，通过标准输入和标准输出交换消息。

```text
MCP Gateway ──启动进程──> filesystem MCP server
          <──stdin/stdout──>
```

优点是本地使用简单；缺点是网关需要管理子进程，并且命令、参数、环境变量都属于敏感配置。

对应代码：`internal/service/mcp/util.go` 中的 `runStdioServer`。

#### Streamable HTTP

客户端通过一个 HTTP MCP 端点通信，是当前项目推荐的远程传输方式。项目入口是 `/mcp`。

对应代码：`createHTTPMcpServerConn` 和 `server.NewStreamableHTTPServer(...)`。

#### SSE

旧版远程传输通常使用一个 SSE 长连接接收服务端消息，再通过另一个 HTTP 端点发送客户端消息。项目保留 `/sse` 与 `/message` 兼容入口，但代码和 CLI 都明确提示 SSE 已弃用，应优先使用 Streamable HTTP。

### 2.6 Stateful 与 Stateless

这里的 Stateful/Stateless 主要是 **MCP Gateway 的上游连接管理策略**，不要简单理解成 HTTP 是否无状态。

- **Stateless**：每次调用创建一个上游 MCP Client，调用结束后关闭。隔离性好，是项目默认值，但频繁初始化可能较慢。
- **Stateful**：同一个上游服务器复用持久连接，适合需要登录状态或冷启动较慢的服务。

项目目前以服务器名作为 Stateful Session 的 key，因此同一个上游服务器只维护一个共享会话。主要代码位于 `internal/service/mcp/session_manager.go` 和 `session_result.go`。

---

## 3. 项目结构：先看什么，暂时跳过什么

```text
main.go                         程序入口
cmd/                            Cobra 命令及服务启动装配
client/                         CLI 调用 MCP Gateway REST API 的客户端封装
internal/api/                   Gin 路由、Middleware、Handler
internal/service/mcp/           最核心：注册、发现、代理和会话管理
internal/service/toolgroup/     Tool Group 管理及子 MCP Server
internal/model/                 GORM 数据库模型
internal/db/                    SQLite/PostgreSQL 连接
internal/migrations/            GORM AutoMigrate
internal/telemetry/             OpenTelemetry/Prometheus 指标
pkg/types/                      对外 API DTO 和枚举
web/dashboard/                  React 管理页面
docs/                           用户文档
```

第一次阅读时可以暂时跳过：

- Dashboard 具体页面和 CSS；
- OAuth 的完整授权细节；
- 所有 CRUD Handler；
- `client/` 中结构相似的 HTTP 方法；
- 大量边界测试。

先抓住 `cmd/start.go → internal/api → internal/service/mcp` 这条主干。

---

## 4. 推荐阅读顺序

### 第 0 步：看产品图，不急着看代码（20 分钟）

阅读：

1. `README.md` 开头至 Quickstart。
2. `assets/mcp-gateway-diagram/april-2026/mcp-gateway-diagram.png`。
3. `docs/core-concepts.mdx`。

目标：能画出“多个 AI 客户端 → 一个 MCP Gateway → 多个上游 MCP Server”。

### 第 1 步：看启动和依赖装配（30～45 分钟）

按顺序阅读：

1. `main.go`
2. `cmd/root.go` 的 `Execute`
3. `cmd/start.go` 的 `runStartServer`
4. `internal/api/server.go` 的 `setupRouter`

`runStartServer` 是全项目的装配中心，做了这些事情：

```text
读取配置和运行模式
    ↓
初始化 OpenTelemetry
    ↓
连接 SQLite/PostgreSQL，执行迁移
    ↓
创建两个 mcp-go Proxy Server
    ↓
创建 SessionManager 和业务 Service
    ↓
创建 Gin Router
    ↓
启动 HTTP Server，监听退出信号并优雅停机
```

读这一段时重点观察依赖是如何显式传入的，而不是只看每个函数的实现。

### 第 2 步：看一次服务器注册（60 分钟）

阅读顺序：

1. `cmd/register.go`：Cobra 如何接收参数。
2. `client/mcp_server.go`：CLI 如何发 REST 请求。
3. `internal/api/server.go`：找到 `POST /api/v0/servers` 路由。
4. `internal/api/mcp_servers.go`：Handler 如何解析输入、创建模型。
5. `internal/service/mcp/server.go`：`registerMcpServer`。
6. `internal/service/mcp/util.go`：如何按照 Transport 建立连接并 initialize。
7. `internal/service/mcp/tool.go`：`registerServerTools`。
8. 再平行查看 `registerServerPrompts` 和 `registerServerResources`。

完整链路：

```text
mcp-gateway register
    ↓ Cobra 解析 --name/--url/--conf
client.Client.RegisterServer
    ↓ POST /api/v0/servers
registerServerHandler
    ↓ 创建 model.McpServer
MCPService.RegisterMcpServerWithOAuthSupport
    ↓
创建上游 MCP Client + initialize
    ↓
保存 McpServer
    ↓
tools/list → 保存 Tool → 加入内存 Proxy Server
prompts/list → 保存 Prompt → 加入 Proxy Server
resources/list → 保存 Resource → 加入 Proxy Server
```

这里需要记住三个设计点：

1. Tool 注册失败不会直接中断其他 Tool；Prompt 和 Resource 整体采用 best-effort 策略。
2. 上游 Tool 在数据库中关联 Server；暴露给下游时名称被改为 `server__tool`。
3. 元数据保存在数据库中，但真正对下游提供能力的是内存中的 `mcp-go` Proxy Server。

### 第 3 步：看一次 Tool 调用（60 分钟）

先看 AI 客户端通过 MCP 协议调用的主链路：

1. `internal/api/server.go` 中的 `/mcp`。
2. `internal/api/middleware.go` 中的 `requireInitialized`、`checkAuthForMcpProxyAccess`。
3. `internal/service/mcp/proxy_filter.go`。
4. `internal/service/mcp/proxy.go` 中的 `MCPProxyToolCallHandler`。
5. `internal/service/mcp/session_result.go` 中的 `getSession`。
6. `internal/service/mcp/session_manager.go`。

调用链如下：

```text
Claude/Cursor/Codex
    ↓ tools/call: context7__get-library-docs
POST /mcp
    ↓ 初始化检查 + MCP Client Token 鉴权
mcp-go Proxy Server
    ↓ 根据工具找到 Handler
MCPProxyToolCallHandler
    ↓ 拆分 context7 和 get-library-docs
检查客户端是否有 context7 访问权限
    ↓
查询上游服务器配置
    ↓
getSession（Stateless 新建 / Stateful 复用）
    ↓
清除下游请求 Header
    ↓
上游 client.CallTool
    ↓
记录调用结果和耗时，返回下游
```

为什么要清除下游 Header：下游传给 MCP Gateway 的认证信息不应继续发送给上游，否则可能泄露 MCP Gateway 的 Client Token。上游所需 Header 应来自该上游服务器自己的配置。

项目还提供 REST 方式的工具调用：`POST /api/v0/tools/invoke`。它最终进入 `internal/service/mcp/tool.go` 的 `InvokeTool`。两条入口不同，但都会完成名称拆分、获取 Session、调用上游和记录指标。

### 第 4 步：看会话管理（30～45 分钟）

重点阅读：

- `internal/service/mcp/session_result.go`
- `internal/service/mcp/session_manager.go`

`sessionResult` 统一包装两种模式：

- Stateless 设置 `shouldClose=true`，调用结束通过 `defer` 关闭；
- Stateful 设置 `shouldClose=false`，连接由 `SessionManager` 持有。

`SessionManager` 内部使用：

```go
map[string]*ManagedSession // key 是 server name
sync.RWMutex
```

Stateful Session 的生命周期：

```text
首次调用 → 创建并缓存
后续调用 → 更新 LastUsedAt 并复用
连接类错误 → InvalidateSession
超过空闲时间 → 定时清理
注销服务器 → CloseSession
进程退出 → Shutdown / CloseAllSessions
```

异常失效使用错误类型和错误文本进行启发式判断，例如 connection reset、broken pipe、EOF、timeout。下一次请求会重新建立连接。

### 第 5 步：看权限与 Tool Group（45～60 分钟）

先区分两类身份：

1. **Human User**：访问 REST 管理 API，有普通用户和管理员角色。
2. **MCP Client**：Claude/Cursor 等连接 `/mcp` 时使用的机器身份，拥有服务器 AllowList。

开发模式下多数鉴权会被跳过；企业模式才要求 Token。

权限代码：

- REST 用户鉴权：`verifyUserAuthForAPIAccess`
- REST 管理员校验：`requireAdminUser`
- MCP Client 鉴权：`checkAuthForMcpProxyAccess`
- 工具发现过滤：`ProxyToolFilter`
- 工具执行校验：`authorizeProxyServerAccess`

权限做了两层防护：

```text
tools/list 阶段：不向客户端展示无权使用的 Tool
tools/call 阶段：即使伪造工具名，也再次校验服务器权限
```

当前 MCP Client 的 AllowList 粒度是 **服务器级**。如果客户端有 `github` 服务器权限，就能访问该服务器公开的所有启用工具。

Tool Group 则用于构造工具子集。一个 Group 可以：

- 直接包含若干工具；
- 包含某个服务器的全部工具；
- 再排除其中若干工具。

每个 Group 会创建独立的内存 MCP Server，通过 `/v0/groups/:name/mcp` 暴露。工具启停或注册变化时，通过回调同步更新 Group。

注意：当前 Group Endpoint 只处理 Tools，不包含 Prompts 和 Resources。

### 第 6 步：看数据库（30 分钟）

阅读：

1. `internal/db/db.go`
2. `internal/migrations/migration.go`
3. `internal/model/mcp_server.go`
4. `internal/model/mcp_tool.go`
5. `internal/model/mcp_client.go`
6. `internal/model/tool_group.go`

数据库选择逻辑：

- 有 `DATABASE_URL` 或 PostgreSQL 环境变量：连接 PostgreSQL；
- 否则使用本地 SQLite 文件；
- 统一通过 GORM 操作；
- 启动时使用 `AutoMigrate` 创建或更新表。

`McpServer.Config` 保存 Transport 相关配置：

- HTTP：URL、Bearer Token、Headers；
- STDIO：Command、Args、Env；
- SSE：URL、Bearer Token。

目前部分凭据会进入数据库配置，其中代码已经留下“应加密存储”的 TODO。这是后续二开可以考虑的安全问题，但在未实现前不要在面试中声称项目已经加密凭据。

### 第 7 步：看 OpenTelemetry 与 Prometheus（30 分钟）

阅读：

1. `internal/telemetry/otel.go`
2. `internal/telemetry/metrics.go`
3. `internal/telemetry/otel_metrics.go`
4. `internal/service/mcp/proxy.go` 中的埋点
5. `cmd/start.go` 中的初始化

这里用了一个值得记住的 No-op 模式：

```text
未启用指标 → 注入 NoopCustomMetrics
启用指标   → 注入 OtelCustomMetrics
业务代码   → 永远调用统一 CustomMetrics 接口，无需反复判断 nil 或开关
```

当前主要指标是：

- `mcp_gateway_tool_calls_total`
- `mcp_gateway_tool_call_latency_seconds`

Label 包括上游服务器、工具名和调用结果。Gin 还接入了 `otelgin` 中间件。启用时，服务在 `/metrics` 暴露 Prometheus 格式指标，生产 Compose 中的 Prometheus 每 15 秒抓取一次。

需要准确表达：项目当前重点使用 OpenTelemetry Metrics，不要说已经实现完整的分布式 Trace。当前 Prompt 调用也复用了同一组计数器和 Label。

### 第 8 步：最后看 Cobra、Dashboard 和 Docker（30～45 分钟）

#### Cobra

`main.go → cmd.Execute → rootCmd.Execute` 是 CLI 入口。每个命令通常包含：

- 命令名称和帮助文本；
- Flags；
- `PreRunE` 参数校验；
- `RunE` 执行业务；
- `rootCmd.AddCommand` 注册命令。

需要注意 `start` 命令直接启动服务；`register`、`list`、`invoke` 等命令则通常通过 `client/` 包调用已运行服务的 REST API。

#### Dashboard

Dashboard 使用 React、TypeScript 和 Vite。构建产物被复制到 `internal/dashboardui/dist`，再通过 Go Embed 打进二进制。第一次读项目时只需理解这条构建关系。

#### Docker Compose

- `docker-compose.yaml`：默认 development 模式。
- `docker-compose.prod.yaml`：默认 enterprise 模式，额外包含 pgAdmin 和 Prometheus。

两份 Compose 当前都启动 PostgreSQL。直接在宿主机运行且没有数据库配置时，应用才会回退到 SQLite。

---

## 5. 对照简历逐条理解

### 简历第 1 条：统一端点与能力聚合

> 面向 Claude、Cursor、Codex 等 AI 客户端提供统一 MCP 接入端点，集中代理和管理多个上游 MCP Server，聚合其 Tools、Prompts 与 Resources，减少客户端重复配置。

对应证据：

- `/mcp` 和 `/sse`：`internal/api/server.go`
- 上游 Server 注册：`internal/service/mcp/server.go`
- Tools 聚合：`internal/service/mcp/tool.go`
- Prompts 聚合：`internal/service/mcp/prompt.go`
- Resources 聚合：`internal/service/mcp/resource.go`

面试时应能回答：

**为什么需要网关？**

多个 AI 客户端不必分别配置每个上游服务器；上游变更、权限和可观测性可以集中管理。

**工具重名怎么办？**

对下游使用 `server__tool` 作为规范名称，调用时再拆回上游服务器名和原始 Tool 名。

### 简历第 2 条：Transport 与 Session

> 支持 Streamable HTTP、SSE 和 STDIO 三种上游传输方式，提供 MCP Server 动态注册、启停和注销能力，并实现 Stateful/Stateless 会话管理、空闲连接回收及异常连接失效重建。

对应证据：

- Transport 枚举：`pkg/types/mcp_server.go`
- 建立三类连接：`internal/service/mcp/util.go`
- 注册/注销/启停：`internal/service/mcp/server.go`
- Session 选择：`internal/service/mcp/session_result.go`
- Session 缓存与回收：`internal/service/mcp/session_manager.go`

面试时应能回答：

**Stateful 为什么快？代价是什么？**

它避免每次重新建立连接和 initialize，但需要处理并发、空闲回收、连接失效以及不同用户间的状态隔离。

**为什么连接异常后不是立即重试 Tool？**

当前项目会让 Stateful Session 失效，使后续调用重建连接。Tool 可能具有非幂等副作用，直接自动重试可能重复发邮件、创建工单或修改数据。

### 简历第 3 条：Gin、Cobra、GORM 与 Docker

> 基于 Gin、Cobra 和 GORM 构建 REST API、命令行工具及持久化层，并提供 Docker Compose 一键运行环境。

对应证据：

- Gin 路由：`internal/api/server.go`
- Cobra 命令：`cmd/root.go` 和 `cmd/*.go`
- GORM 模型：`internal/model/`
- 数据库连接：`internal/db/db.go`
- 迁移：`internal/migrations/migration.go`
- 容器编排：`docker-compose.yaml`、`docker-compose.prod.yaml`

面试时不要只背框架名称，要能说清职责：

- Gin 负责 HTTP 路由和中间件；
- Cobra 负责 CLI 命令、参数、帮助信息和执行分发；
- GORM 负责模型映射和数据库访问；
- Docker Compose 编排网关、PostgreSQL、Prometheus 等服务。

### 简历第 4 条：鉴权、隔离与可观测性

> 支持 Access Token 鉴权、客户端服务器级访问控制和 Tool Group 工具隔离；集成 OpenTelemetry/Prometheus，采集工具调用量、执行结果及延迟指标。

对应证据：

- Token 中间件：`internal/api/middleware.go`
- MCP Client AllowList：`internal/model/mcp_client.go`
- Tool 发现过滤：`internal/service/mcp/proxy_filter.go`
- Tool Group：`internal/service/toolgroup/toolgroup.go`
- 指标接口与实现：`internal/telemetry/metrics.go`、`otel_metrics.go`
- 调用埋点：`internal/service/mcp/proxy.go` 和 `tool.go`

面试时应能区分：

- User Token 用于 REST API；
- MCP Client Token 用于 `/mcp`；
- AllowList 控制客户端能访问哪些服务器；
- Tool Group 创建只暴露特定工具的独立 MCP Endpoint。

---

## 6. 建议亲手跟踪的两个调试场景

### 场景 A：注册一个上游服务器

在以下位置设置断点：

1. `cmd/register.go`：`runRegisterMCPServer`
2. `internal/api/mcp_servers.go`：`registerServerHandler`
3. `internal/service/mcp/server.go`：`registerMcpServer`
4. `internal/service/mcp/util.go`：对应 Transport 的连接函数
5. `internal/service/mcp/tool.go`：`registerServerTools`

观察：

- initialize 后拿到了哪些 capabilities；
- `tools/list` 返回了什么；
- Tool 在数据库中的名称和暴露给下游的名称有什么区别；
- Tool 如何被加入内存 Proxy Server。

### 场景 B：从 AI 客户端调用一个 Tool

设置断点：

1. `internal/api/middleware.go`：`checkAuthForMcpProxyAccess`
2. `internal/service/mcp/proxy.go`：`MCPProxyToolCallHandler`
3. `internal/service/mcp/session_result.go`：`getSession`
4. `internal/service/mcp/session_manager.go`：`GetOrCreateSession`
5. `internal/telemetry/otel_metrics.go`：`RecordToolCall`

观察：

- Context 中的 `mode` 和 `client` 从哪里来；
- `server__tool` 如何拆分；
- Stateful 和 Stateless 分别走哪条分支；
- 请求 Header 为什么被清空；
- 成功或失败如何写入指标。

---

## 7. 本地运行和测试前的注意事项

如果只是快速体验，优先使用仓库提供的 Docker Compose 和已构建镜像，能避免本机工具链问题。

如果要从源码运行，需要先构建 Dashboard，因为 `internal/dashboardui/embed.go` 使用 `go:embed dist dist/*`：

```bash
bash scripts/build-dashboard.sh
go run . start
```

构建脚本会执行 `npm ci`、TypeScript 检查、Vite 构建，并把产物复制到 `internal/dashboardui/dist`。

当前在 Windows、`CGO_ENABLED=0` 环境直接执行 `go test ./...` 可能遇到：

1. Dashboard `dist` 不存在导致 Embed 编译失败；
2. 部分测试使用 `gorm.io/driver/sqlite`，在该环境中要求 CGO；
3. `cmd/config` 测试通过修改 `HOME` 隔离配置目录，但 Windows 的 `os.UserHomeDir()` 不一定读取 `HOME`。

这些是开发环境与测试可移植性问题，不能直接据此认定核心网关逻辑失效。

---

## 8. 四小时速读计划

如果很快就要面试，可按以下顺序：

| 时间 | 内容 | 达成目标 |
| --- | --- | --- |
| 0:00～0:30 | 第 1～2 章 | 说清 MCP 和网关角色 |
| 0:30～1:00 | `runStartServer`、`setupRouter` | 画出组件关系 |
| 1:00～2:00 | 注册链路 | 说清能力如何被发现和聚合 |
| 2:00～3:00 | Tool 调用与 Session | 说清路由、连接复用和异常恢复 |
| 3:00～3:30 | 鉴权与 Tool Group | 说清两类 Token 和两层校验 |
| 3:30～4:00 | Metrics、简历自问自答 | 能解释简历所有关键词 |

第一遍不要追求记住全部函数。每看完一条链路，关掉代码，尝试自己画一次流程图；画不出来再回代码补缺口。

---

## 9. 面试前自检问题

如果下面的问题能用自己的话回答，这个项目就基本能进入简历面试阶段：

1. MCP Gateway 为什么既是 MCP Server 又是 MCP Client？
2. Tool、Resource、Prompt 分别表示什么？
3. 注册上游 MCP Server 时发生了哪些步骤？
4. 为什么工具名称要变成 `server__tool`？
5. 一次 `tools/call` 如何找到正确的上游服务器？
6. Streamable HTTP、SSE、STDIO 有什么区别？
7. Stateful 和 Stateless 在这个项目中具体指什么？
8. Stateful Session 在什么时候创建、复用、失效和关闭？
9. 为什么下游请求 Header 不能直接转发给上游？
10. Human User 和 MCP Client 有什么区别？
11. Tool 发现阶段和调用阶段为什么都要做权限检查？
12. Tool Group 与 MCP Client AllowList 有什么区别？
13. OpenTelemetry 和 Prometheus 各自承担什么角色？
14. 为什么 Metrics 关闭时还要注入一个 No-op 实现？
15. SQLite 和 PostgreSQL 的选择逻辑是什么？
16. 为什么不能轻易自动重试失败的 Tool 调用？
17. 进程收到 SIGTERM 后如何优雅退出？
18. 当前项目有哪些限制或值得二次开发的地方？

最后一题可以忠实回答：细粒度 ACL、凭据加密、下游 OAuth/OIDC、Prompts/Resources 的 Group 支持、会话隔离、熔断限流以及开发测试可移植性，都还有改进空间。
