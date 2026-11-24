# agentd 运行与运维

本文描述当前代码的启动、接口、权限与故障处理，随代码更新。

实现依据：[第一阶段架构基线](../design/01-stage-1-architecture.md)。核心代码位于一个 `internal/agent` 包，数据库为四张表，当前 schema v4 共 37 列。运行平台是 macOS / Linux；没有 Redis、外部数据库或容器依赖。模型通过 OpenAI 兼容的 Chat Completions API 连接。

**启动与本地依赖**

```sh
./scripts/bootstrap
make build
./bin/agentd -config config.yaml
```

运行状态都在状态根目录 `.agentd/` 下（`-root` 可改），本文出现的 `data/`、`workspace/` 都指其中的目录。

Go 工具链保存在 `.tools/go/`，模块缓存、构建缓存、临时文件都在 `.cache/`。`scripts/go` 设置这些路径，不修改系统 Go 或用户全局 Go 环境。已安装本地工具链和下载模块后，构建可离线进行。工具链版本在 bootstrap 中固定，下载后核验官方 SHA-256；依赖版本由 go.mod/go.sum 固定。

默认监听 `127.0.0.1:8090`；本地模型地址为 `http://localhost:8080/v1/chat/completions`，模型名称在 config.yaml 中配置。初次启动自动生成 `data/api-token`，权限为 0600；管理客户端使用 Bearer Token，不输出 Token 到日志。配置私密凭证时使用被 Git 忽略的 `config.local.yaml`，并以 `-config config.local.yaml` 启动；该文件是完整配置，不是叠加文件。

```sh
TOKEN=$(cat .agentd/data/api-token)
curl -s http://127.0.0.1:8090/healthz
curl -s http://127.0.0.1:8090/v1/events \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"key":"hello-001","text":"请调用 send_message 给我发送一句你好。"}'
curl -s http://127.0.0.1:8090/v1/outbox -H "Authorization: Bearer $TOKEN"
```

默认 `local` 投递器将已发通知保存到 `data/deliveries/*.json`，每个通知使用稳定文件名，方便本地查看。它不是 Slack/飞书发送器。需要外部通知时配置 `kind: webhook`，接收服务接受 JSON POST `{id,key,target,content}`；原生 Slack、飞书、Telegram 协议适配器未包含在此版本中，可通过接收桥接服务接入。

普通通知默认遵循北京时间 23:00–08:00 静默期，审批提示不受静默期影响。需要立刻体验本地通知时，将配置中的 quiet_hours.start / end 都设为空字符串并重启。不要删除数据库来“重试”通知。

带工作台的发布版公开提供页面和静态资源，业务 API 复用同一监听地址。工作台无需登录，能够访问该入口的人均可查看会话并审批动作。工作台请求的 Host 和写操作的 Origin 须与监听地址或显式配置的 `web_origin` 匹配，CLI 可省略 Origin。所有者可从 `/v1/conversations`、`/v1/runs`、`/v1/deliveries` 浏览历史，按返回的运行 ID 或消息 ID 打开 `/v1/runs/{id}`、`/v1/replies/{id}` 核对上下文和回执。在「IM 渠道」可查看脱敏身份、连接和未结工作量。GET `/v1/im` 返回当前修订号；POST `/v1/im` 使用相同修订号提交 `save`、`select` 或 `remove`，过期修改返回 409。页面保存后按提示重启 agentd，使运行进程加载新配置。详见[工作台说明](../../web/README.md)。

**HTTP 接口**

JSON 请求必须包含 Content-Type: application/json，并携带认证。未知字段会被拒绝；请求大小受配置限制。渠道适配器应在收到成功响应后才确认业务事件；同 key 同内容可安全重投，同 key 不同内容返回 409。

| 方法与路径 | 输入或用途 |
| --- | --- |
| GET /healthz | 数据库健康检查，无认证，不暴露配置 |
| GET /v1/workbench | `kind=actions/inbox/outbox/drafts`，`offset` 默认 0；返回全部类别的待办数和所选类别的一页（最多 20 项） |
| GET /v1/actions/{id} | 单个动作的脱敏冻结参数、状态与结果，用于审批后核对 |
| GET /v1/chats、POST /v1/chats | 分页浏览对话、分配新网页会话标识 |
| GET /v1/chats/{id}/messages、POST /v1/chats/{id}/messages | 按游标查询网页会话的输入、回复和审批，使用稳定 key 提交消息 |
| GET /v1/chats/{id}/submissions/{key} | 查询发送回执，id 为 0 表示暂未查到，无需触发模型 |
| GET /v1/status | 暂停、执行状态、各表条数 |
| GET /v1/inbox | 最近 200 条事件 |
| GET /v1/timers | 最近 200 个定时器 |
| GET /v1/actions | 最近 200 个动作及冻结内容、结果 |
| GET /v1/outbox | 最近 200 条通知及投递状态 |
| POST /v1/events | `{key,text}`：普通人工消息 |
| POST /v1/timers | `{id,cron_expr,fire_at,prompt,active}`，fire_at 为 Unix 毫秒 |
| POST /v1/control | `{command:"pause" / "stop" / "resume"}` |
| POST /v1/knowledge/uploads?key={32 位十六进制编号} | multipart 上传，第一项 selection 为按序的文件说明数组，随后每项 file 为原件。完整批次持久化后返回 202。相同编号与相同文件重放返回已有回执 |
| GET /v1/knowledge/uploads、GET /v1/knowledge/uploads/{id} | 可用格式和最近 20 批摘要，或按编号核对逐项转换及入库结果 |
| GET /v1/knowledge/search?q=词句、GET /v1/knowledge/{id} | 检索当前入库正文、按文档编号查看正文 |
| POST /v1/knowledge/incoming | 工作台高级导入，path 相对于服务器状态根目录的 data/knowledge/incoming/ |
| GET /v1/evolution、GET /v1/evolution/document、GET /v1/evolution/uses | 当前记忆、技能、整理任务与审核情况；后两项以 path 选择条目。document 可用 at 读取早期版本，uses 返回运行时留存的依据 |
| POST /v1/evolution/dream、/v1/evolution/edit、/v1/evolution/review | 按模型和请求编号发起整理、按版本保存人工修订、按审阅时的基线决定候选或草稿 |
| GET /v1/knowledge | 已导入的文档清单、是否正在导入、上一次导入每个文件的结果 |
| POST /v1/knowledge/import | `{path}`：后台导入一个文件或目录（相对路径按 agentd 的工作目录）；已有导入在运行时返回 409 |
| POST /v1/knowledge/{doc_id}/remove | 归档一个文档并移出索引；导入运行时返回 409 |
| GET /v1/drafts | 等待审核的 skill 草稿与规则提议，含批准时会遇到的问题 |
| POST /v1/drafts/{name}/approve | 批准：skill 安装到 `workspace/skills/<name>/`（旧版本留在 `data/gate/versions/`）；`identity` 草稿追加到 IDENTITY.md |
| POST /v1/drafts/{name}/reject | `{reason}` 必填；草稿与理由移到 `skills-drafts/.rejected/`，供下次整理参考 |
| POST /v1/memory/{op} | 记忆整理结果：`promote` 提升待确认的候选，`discard` 丢弃它，`rollback` 回到上一版本（当前版本留作版本，可再次回滚撤销）。整理运行中、候选缺少基线或当前记忆已变化时返回 409。过期的待确认候选会在下一次整理开始时自动丢弃（指标记为 `discarded`）；未过期的待确认候选会让后续整理跳过，直到被处理。`GET /v1/status` 的 `memory.held_stale` 显示是否已过期 |
| POST /v1/events/{id}/resolve | `{operation:"retry" / "skip"}`：重试 dead 或显式跳过 dead/pending |
| POST /v1/action-requests | `{key,name,arguments,text}`：提交已配置的命名外部动作 |
| POST /v1/actions/{id}/decision | `{decision:"approve" / "reject"}` |
| POST /v1/actions/{id}/resolve | `{status:"succeeded" / "failed",evidence:"核对证据"}`：仅处理 unknown |
| POST /v1/outbox/{id}/resolve | `{status:"sent" / "pending" / "failed"}`：人工处理 unknown/failed |

Outbox 的 pending 解析意味着操作者已确认未送达、允许重新投递。确认不清楚时保留 unknown。动作 unknown 核对不会自动再次执行远端写入；确认失败后如确实要执行，另发新的明确请求。

`stop` 与 `pause` 均持久化暂停并取消活跃 Run，必须 resume 才继续，避免“刚停止又自动领取原事件”。正在运行的 HTTP 外部动作已被标记为 executing；暂停不会假装撤销在途远端效果。控制与状态提交使用同一把宿主锁，远端响应仍回写事实。服务收到 SIGINT/SIGTERM 时停止后台循环，等待取消与状态保存，再释放文件锁和数据库。

**定时器与顺序**

- 五字段 Cron 使用配置时区，例如 `0 9 * * 1-5`。重启错过多次周期时合并为一次唤醒，再计算未来发生时间，不补发每一次历史周期。
- 一次性定时器要求 fire_at；周期定时器可省略 fire_at，自动计算下一次。active=false 取消未来触发，但不自动撤销已经入 Inbox 的发生事件。
- 普通消息在固定一秒窗口内合批，窗口不滑动；审批和定时器事件不参与普通消息合批。批次最大条数及输入总大小受限。
- 全局按序领取；退避或 dead 队头会阻止后续业务事件，需认证操作显式 retry/skip。暂停和审批决定的记录不依赖模型，恢复业务执行仍遵守队列顺序。
- 每个事件最多三次执行失败，取消不计失败。每小时 Run 启动量、每轮步数、时间、模型输出和工具输出都有上限；模型接口故障不会无限自激调用。

**六个工具与权限**

| 工具 | 行为 |
| --- | --- |
| read_file | 从 workspace 或受控 outputs 引用读取，返回内容预览、SHA-256 和下一偏移量 |
| search | 在 `sessions/`（运行记录，最新的一天在前）和 `notes/` 中查找同一行包含所有关键词的内容（不区分大小写），返回路径、偏移和片段，最多 20 条；再用 `read_file` 按偏移读上下文。群聊轮次不能检索运行记录 |
| write_file | 原子替换 MEMORY.md、notes/ 或 scratch/；已存在文件要求 expected_hash，新文件使用 absent |
| edit_file | 用当前 expected_hash 替换一次唯一匹配的文本 |
| set_timer | 将变更暂存到当前 Run，成功提交时与 Inbox 完成同事务保存 |
| send_message | 暂存通知，成功提交时入 Outbox；不代表已经送达 |
| bash | 隔离执行 command；或使用 action / arguments 调用宿主命名 HTTP 操作，两种形式互斥 |

IDENTITY.md 与 skills/ 对 Agent 只读；目录穿越、指向工作区外的符号链接不允许。文件读写通过 Go os.Root 限定范围。普通文件写入可以在重跑时重复，替换内容与版本检查使常见重放安全；它不是通用文件事务或任意脚本回滚。

默认 bash command 被禁止。启用时 macOS 使用 sandbox-exec，Linux 必须已安装 bubblewrap，缺失则明确拒绝，不退化为宿主裸 shell。沙箱只读工作区，仅当前 scratch 子目录可写，不能读取 data/ 或宿主凭证，不能直接访问网络。macOS 原生沙箱平台能力可能随系统更新变化，应运行对应隔离测试后再启用。

例如，宿主配置一个写接口：

```yaml
actions:
  create_ticket:
    mode: ask
    url: http://127.0.0.1:9090/tickets
    method: POST
    bearer_token: your-private-token
```

模型通过 bash 工具提交 `{"action":"create_ticket","arguments":{"title":"example"}}`。这不是 shell 字符串，不在子进程中执行；URL、HTTP 方法由配置固定，参数原样冻结。未知操作拒绝；只读 GET 使用固定 URL 和空参数，不接受模型传入任意目的地址。HTTP 重定向不自动跟随，防止凭证被重定向带往其他主机。

默认基础文件/定时/通知工具只有 allow/deny；ask 专用于命名外部写动作。不能批准一段可变 shell 脚本后宣称其内部每次网络操作均已授权。

**动作、审批和故障恢复**

allow 外部写与 ask 获批写都先登记 actions，再将 approved 原子改为 executing，随后才发起远端请求；成功回填 succeeded/result。执行意图键绑定原事件和命名操作，不绑定可能被模型重排的工具序号；同一原事件内每种命名写操作只接受一组冻结参数，重复相同调用复用回执，改变参数则拒绝继续。如需用同一操作执行多个不同写入，应提交独立事件。审批恢复中的相同操作仍复用原动作键。本地默认投递目标允许通知时，若模型在恢复后直接结束，宿主会向本地 Outbox 写入带动作 ID 去重的执行回执。相同未核对的写意图也不能通过新事件绕过 unknown。

ask 在同一事务中提交 pending_approval 和输入完成。网页任务由原会话展示审批入口，其他入口沿用审批通知机制，结束本轮。批准/拒绝接口校验现状态与到期时间并生成稳定事件。批准后校验当前配置、冻结目的地址和有效期，执行一次，再让模型根据事实继续。拒绝与过期同样生成恢复事件。自动 allow 动作也给出未来有效期；没有采用设计中“expires_at=当前时间”的字面写法，否则统一的到期校验会立即拒绝它。

外部返回明确拒绝状态时记 failed；网络异常、5xx、重定向及无法确定是否产生副作用的结果保留 unknown。接入真实业务时，应校验目标 API 的错误语义；通用 HTTP 适配器不会证明第三方副作用恰好一次。投递 429 有界退避，其他不明确结果也不会盲目重发。

重启将 running 输入记一次失败退回 pending/dead，将 executing 动作和 sending 通知转为 unknown。单实例文件锁防止同数据库双宿主；不支持共享网络文件系统或跨机器接管。运行中的 shell 用独立进程组取消并等待退出；宿主被 SIGKILL 后的子进程治理仍需 systemd/容器/launchd，不承诺仅凭文件锁寻回孤儿。

**记忆、文件和运维**

`data/session.jsonl` 是有限的自然语言上下文缓存，忽略残损行，超限保留最近完整记录，不作为执行证据；本版采用滑动截断，未引入额外的模型摘要调用。长期信息由 Agent 通过 MEMORY.md 和 notes/ 保存。工具调用与送达事实始终查询数据库。

已登记 actions/result 与发送状态不会因缓存损坏而丢失。模型轮内的通知和定时器在内存中暂存，失败不会提交；本地笔记替换、已经产生的远端副作用不能随 Run 失败回滚。`data/outputs/` 单次最多保留 1 MiB，模型仅接收短预览；总目录保留策略由操作者安排，当前版本没有后台自动清理审计数据。

密钥、数据库、工具输出、缓存和实际笔记被 Git 忽略。日志不包含 API Token 或配置中的 Bearer Token。备份应暂停并停止进程后复制 data/ 与 workspace/；运行中不要只复制 state.db 而丢弃 WAL 文件。修改配置需重启；有未完成动作或通知时，应先核对影响，不随意替换接收目的地。

**测试与证据**

```sh
make test       # SQLite、HTTP、工具与故障恢复测试；不调用真实模型
make race       # 数据竞争检测，需要平台 C 编译器
make vet
make live       # 真实本地模型、完整服务、项目内临时数据、loopback 写接口
python3 scripts/smoke.py  # 已构建二进制的启动、真实模型、暂停与重启验证
```

真实模型测试主动调用本机模型，会消耗本机推理资源，但不发送外部业务消息。报告、数据库和本地投递文件保留在 `.cache/live-e2e/`；二进制测试数据位于 `.cache/binary-e2e/`。普通测试临时目录也在项目 `.cache/tmp/`，结束后自动清理。

实现参考了 Go 官方的 [事务使用规则](https://go.dev/doc/database/execute-transactions)、[目录范围文件访问](https://go.dev/blog/osroot)、[数据竞争检测](https://go.dev/doc/articles/race_detector)，SQLite 驱动的 [连接参数文档](https://pkg.go.dev/modernc.org/sqlite) 和 [robfig/cron 的时区与解析器说明](https://github.com/robfig/cron)。代码使用短事务、参数化 SQL、Context、固定连接池和原生工具调用，未引入额外 Agent 编排框架。
