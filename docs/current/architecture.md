# 当前软件架构

本文描述**当前代码**的结构，随代码更新。设计依据见 [第一阶段基线](../design/01-stage-1-architecture.md) 和 [第二阶段记忆设计](../design/02-stage-2-memory.md)；路径的目标布局见 [90-convention-storage-layout](../design/90-convention-storage-layout.md)，本文只写现在的实际路径。

## 一、总览

一个 Go 进程（`cmd/agentd`），只有一个业务包 `internal/agent`。进程启动后运行 HTTP 服务和四个后台循环，所有状态存放在 SQLite 的 4 张表和两个目录里（`data/`、`workspace/`）。

```text
                  外部输入                                          外部输出
   ┌──────────────┬──────────────┬──────────────┐       ┌──────────────┬──────────────┬──────────────┐
   │  HTTP API    │  IM channel  │   Timers     │       │  IM channel  │   webhook    │ local file   │
   │  http.go     │ dingtalk.go  │  timers.go   │       │  (deliver)   │              │ data/        │
   │              │ wecom.go     │  (cron)      │       │              │              │ deliveries/  │
   └──────┬───────┴──────┬───────┴──────┬───────┘       └──────▲───────┴──────▲───────┴──────▲───────┘
          │              │              │                      └──────────────┼──────────────┘
          ▼              ▼              ▼                                     │
   ┌─────────────────────────────────────────────┐            ┌───────────────┴───────────────┐
   │ Event Inbox   inbox.go      table: inbox    │            │ Outbox  outbox.go table:outbox│
   │ 去重 → 排队 → claim 一批 → 提交或重试          │            │ 静默时段 / 重试 / unknown 保护   │
   └──────────────────────┬──────────────────────┘            └───────────────▲───────────────┘
                          │ batch                                             │ notifications
                          ▼                                                   │ (同一事务)
   ┌──────────────────────────────────────────────────────────────────────────┴───────────────┐
   │ Agent loop   loop.go                                                                      │
   │   ① runBusiness（news.go）命中业务事件 → 直接处理，不进模型                                     │
   │   ② 组装上下文：system prompt（memory.go）+ 历史 + 本批事件                                    │
   │   ③ 执行器：原生循环（model.go → 本地模型）  或  OpenCode（opencode.go，经临时 MCP 回调工具）      │
   │   ④ 工具调用 tool() → 权限检查 → 6 个基础工具 + 命名 HTTP 操作                                  │
   └───────┬─────────────────────┬──────────────────────┬───────────────────────┬─────────────┘
           │                     │                      │                       │
           ▼                     ▼                      ▼                       ▼
   ┌───────────────┐   ┌──────────────────┐   ┌──────────────────┐   ┌────────────────────┐
   │ Memory        │   │ Permissions      │   │ Sandbox          │   │ Timers             │
   │ memory.go     │   │ permissions.go   │   │ sandbox.go       │   │ set_timer → 表     │
   │ workspace/    │   │ table: actions   │   │ bash（默认 deny） │   │ timers             │
   │ data/sessions/│   │ 审批 / 冻结请求    │   │                  │   │                    │
   └───────────────┘   └──────────────────┘   └──────────────────┘   └────────────────────┘
```

后台循环在 `service.go` 的 `Start` 中启动：`workerLoop`（Inbox）、`timerLoop`、`outboxLoop`、`im.run`（当前 IM 渠道的长连接）。

## 二、模块与文件

按 [AGENTS.md](../../AGENTS.md) 的架构不变量，核心文件不认识具体渠道和业务（`boundary_test.go` 检查）；渠道通过 `imChannel` 接口接入，业务通过 `runBusiness / admitBusiness / timerEvent` 三个入口接入。

| 层 | 文件 | 职责 |
| --- | --- | --- |
| 入口 | `cmd/agentd/main.go` | 读配置、`Open`、启动 HTTP 和后台循环、优雅退出 |
| 核心：五原语 | `inbox.go` | 事件写入（去重键）、按顺序 claim、执行、提交或退回重试、死信 |
| | `timers.go` | cron / 一次性定时器，到点把 prompt 写成 Inbox 事件 |
| | `memory.go` | 系统提示组装（稳定前缀）、`MEMORY.md` 上限、运行记录 `data/sessions/`、工作区读写 |
| | `permissions.go` | 工具权限、命名 HTTP 操作的冻结请求、审批、执行与结果未知处理 |
| | `outbox.go` | 通知排队、静默时段、按目标投递（im / webhook / local）、重试 |
| 核心：运行 | `loop.go` | 一次运行：组装消息、模型循环、6 个工具的分发、分页读取 |
| | `model.go` | OpenAI 兼容的 chat completions 客户端、工具定义 |
| | `opencode.go` | 可选执行器：启动 `opencode run`，用临时 MCP 接口把工具调用转回 `tool()` |
| | `sandbox.go` | `bash` 工具的受限执行 |
| | `store.go` | SQLite 打开（写库单连接 + 只读库）、4 张表、Payload 格式 |
| | `service.go` | `Service` 结构、启动/关闭、暂停、脱敏、原子写 |
| | `http.go` | 管理 API：事件、定时器、审批、对账、状态、列表 |
| | `config.go` | 配置结构、默认值、校验 |
| 渠道 | `im.go` | `imChannel` 接口、IM 路由、群聊判定、团队提示、渠道脱敏 |
| | `dingtalk.go` / `wecom.go` / `im_phrases.go` | 钉钉 Stream、企微长连接的具体实现 |
| 业务 | `news.go` / `anthropic_news.go` | AI 资讯：拉取 RSS / Newsroom、去重、翻译标题、推送 |

**4 张表**（`data/state.db`）：

| 表 | 存什么 | 状态 |
| --- | --- | --- |
| `inbox` | 待处理事件（payload、去重键、可用时间、重试次数） | pending → running → done / dead |
| `timers` | 定时器（cron 或一次性时间、prompt、是否启用） | active 0/1 |
| `actions` | 需要权限的命名操作（冻结的参数、恢复提示、结果） | pending_approval → approved/rejected → executing → succeeded/failed/unknown |
| `outbox` | 待投递通知（目标、内容、投递时间） | pending → sending → sent / failed / unknown |

**6 个工具**：`read_file`、`write_file`、`edit_file`、`set_timer`、`send_message`、`bash`（默认 deny）；配置中的命名 HTTP 操作也作为工具暴露给模型。

## 三、数据流

### 1. 一个事件的生命周期

```text
HTTP POST /v1/events ─┐
IM 消息（私聊/群 @）  ─┼─► Append ──► inbox(pending)        去重键相同只写一次
定时器到点            ─┘     │         admitBusiness 可改写或拒绝业务事件
                             ▼
                  workerLoop: 按 id 顺序 claim → running
                    连续的普通消息可合成一批（≤ max_batch、≤ max_input_bytes）
                    队首是死信时整个队列停住，等人工处理
                             │
                             ▼
                  execute ──► runBusiness 命中？──是──► 业务直接产出 Outcome
                             │否
                             ▼
                  模型循环（最多 max_steps 步，上下文 ≤128KB）
                    model.Complete ─► tool_calls ─► tool() ─► 结果回给模型
                             │                         │
                             │                         ├─ 读写工作区 / 读运行记录
                             │                         ├─ 需审批 → Outcome.Approval，本轮结束
                             │                         └─ send_message / set_timer → 记入 Outcome
                             ▼
                  Outcome = { 通知, 定时器, 待审批操作, 回答 }
                             │
                             ▼
                  commitBatch：一个事务内
                    写 outbox + 写 timers + 写 actions(+审批卡片) + inbox → done
                             │
                             ▼
                  recordRun → data/sessions/YYYY-MM-DD.jsonl（脱敏）
                             │
                             ▼
                  outboxLoop: deliverOne → im.deliver / webhook / data/deliveries/
```

失败时 `returnBatch` 把事件退回 pending 并按退避时间重试；第 3 次失败变为 dead（IM 来源会通知用户），需用 `POST /v1/events/{id}/resolve` 人工处理。

### 2. 审批续跑

```text
tool() 调用命名操作 ─► permissions: 需要审批 ─► actions(pending_approval) + 审批通知
                                                     │
                     POST /v1/actions/{id}/decision ◄┘ （人工）
                                │
                                ▼
                 Append 一个 kind=approval 的事件 ─► 下一轮 execute:
                 executeAction 执行冻结请求（只执行一次，结果未知时标 unknown）
                 ─► 把回执交给模型，继续原来的任务
```

### 3. 记忆（第二阶段，部分实现）

```text
             每次运行                                        每次运行开始
   recordRun / recordTool ──► data/sessions/*.jsonl         systemPromptFor:
   （只追加，模型只读）            │                            静态头 → 记忆指南 → IDENTITY.md
                                  │ read_file sessions/...    → MEMORY.md（≤200 行 / 8KB）
                                  ▼                           → 命名操作 → 技能提示 → 当前时间
         记忆整理定时器 ──► 模型按 skills/memory-consolidation/SKILL.md
                            检查、修正、合并 ──► workspace/MEMORY.md + workspace/notes/
```

原始数据（运行记录）只增不改；记忆（`MEMORY.md`、`notes/`）由模型维护，代码只限制上限。整理属于"关键动作"，设计上应使用最强档外部模型，这部分尚未实现。

### 4. 两种执行器

| | 原生循环（默认） | OpenCode（配置 `opencode_binary`） |
| --- | --- | --- |
| 推理与工具循环 | `loop.go` 调 `model.go` | `opencode run` 子进程 |
| agentd 工具 | 直接调用 `tool()` | 经本机临时 MCP 接口回调 `tool()`，权限与审批相同 |
| 额外能力 | — | OpenCode 自带工具（如 websearch），结果写入运行记录 |
| 会话 | 所有者：`data/session.jsonl`；IM：从 inbox/outbox 恢复 | 每个 IM 会话固定一个 OpenCode session，数据在 `data/opencode/` |

## 四、运行状态在哪里

当前使用 `-root .`，即仓库根目录（目标是迁入 `.agentd/`，见 [90-convention-storage-layout](../design/90-convention-storage-layout.md)）：

| 路径 | 内容 | 谁写 |
| --- | --- | --- |
| `data/state.db` | 4 张表 | 核心 |
| `data/api-token` | 管理 API 的 Bearer Token | 首次启动生成 |
| `data/sessions/` | 运行记录（按天 JSONL） | 只有 `appendTranscript` |
| `data/session.jsonl` | 所有者对话的近期上下文 | `memory.go` |
| `data/deliveries/` | `local` 目标的投递结果 | `outbox.go` |
| `data/opencode/` | OpenCode 会话数据 | OpenCode |
| `workspace/IDENTITY.md`、`MEMORY.md`、`notes/`、`skills/`、`scratch/` | 模型可读写的工作区 | 模型（经工具） |
