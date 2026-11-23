# agentd

一个使用 Go 实现的单机 Always-on Agent，保留 Event Inbox、Timers、Memory、Permissions、Outbox 五个组件。核心采用一个业务包、SQLite 四张表和七个基础工具。

## 启动

```sh
./scripts/bootstrap
make build
./bin/agentd -config config.yaml
```

- 服务：`http://127.0.0.1:8090`，健康检查 `/healthz`。
- 模型：`http://localhost:8080/v1/chat/completions`，模型名称见 `config.yaml`。
- 运行状态：全部位于状态根目录 `.agentd/`（`-root` 指定，默认 `.agentd`），下文的 `data/`、`workspace/` 都指其中的目录。见[运行状态目录与路径约定](docs/design/90-convention-storage-layout.md)。
- API 密钥：首次启动生成 `.agentd/data/api-token`，除健康检查外均需 Bearer Token。
- 默认投递：保存到 `data/deliveries/`；普通通知遵循配置中的夜间静默时段。
- **从旧布局升级**：旧版本把 `data/`、`workspace/` 放在仓库根目录。检测到旧布局时，默认启动会停止并提示：执行 `mkdir .agentd && mv data workspace .agentd/` 完成迁移，或加 `-root .` 继续使用旧布局。程序不会自动移动数据。
- Go 工具链、依赖、构建缓存和测试数据均保存在项目 `.tools/`、`.cache/` 中。

```sh
TOKEN=$(cat .agentd/data/api-token)
curl -s http://127.0.0.1:8090/v1/events \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"key":"hello-001","text":"请用 send_message 发送一句你好。"}'
curl -s http://127.0.0.1:8090/v1/outbox -H "Authorization: Bearer $TOKEN"
```

## 测试

```sh
make test
make race
make vet
make live
python3 scripts/smoke.py
make dingtalk-test
make dingtalk-live
```

`make live` 和二进制测试使用真实本地模型，只向本地测试接收器发送请求，报告位于 `.cache/live-e2e/` 和 `.cache/binary-e2e/`。`make dingtalk-live` 使用真实本地模型与模拟钉钉传输，报告位于 `.cache/dingtalk-e2e/`；它不代表真实钉钉云端验收。其他测试不依赖模型服务。

## 记忆

- **运行记录**：每次运行的输入、工具调用和结果、回答、状态，脱敏后追加到 `data/sessions/YYYY-MM-DD.jsonl`（按配置时区分天）。每条记录带 `role`（目前固定 `default`）、`principal`（`local` 本机与定时任务、`owner` 所有者私聊、`user-<hash>` 其他人私聊、`group-<hash>` 群聊、`unknown` 无法确定来源）和 `conversation`（会话的哈希），不写平台原始 ID。OpenCode 自带的工具（如 websearch）只能记下 OpenCode 事件里报告的名称、状态、输入，以及截断到 2KB 的输出或失败原因，事件里没有的内容记录不到。模型用 `search` 按关键词检索运行记录和笔记（最新的一天在前），再用 `read_file` 按返回的偏移分页细读；不能修改，读取和检索本身不会写入记录；回复会被其他群成员看到的轮次（钉钉群、企微群）不能读取或检索运行记录。文件只增不减，备份时包含 `data/sessions/`。
- **记忆文件**：`workspace/MEMORY.md` 是每次运行都会加载的索引，由模型维护，超过 200 行或 8KB 的写入会被拒绝；细节放在 `workspace/notes/`。
- **整理**：ID 为 `memory-consolidation` 的定时器触发整理，agent 按内置的 `skills/memory-consolidation/SKILL.md`（源文件在 `internal/agent/defaults/skills/`；在 `workspace/skills/` 下放同名文件可覆盖）对照运行记录检查、修正、合并记忆，进度记在 `notes/memory-consolidation.md`。整理会改写之后每次运行都会读到的记忆，属于关键动作：
  - **只由 strong 档模型执行**，在配置的 `strong_model` 中填写任一 OpenAI 兼容接口（如 OpenAI、Gemini 的 chat completions 地址）并设 `enabled: true`。未开启或调用失败时本次整理跳过（运行记录状态 `skipped`），不会改用本地模型，也不会重试。
  - **开启即同意外发**：整理时所有者私聊和本机运行的记录、记忆文件、skill 和已导入的 knowledge 会发往该服务；群聊、其他用户和旧记录不会发送。
  - 整理只能用读写文件的工具，不能发消息、设定时器或执行命令。
  - **先改候选，检查通过才生效**（[设计](docs/design/02-stage-2-memory.md#整理的安全机制)）：整理只改 `data/memory/candidate/` 里的副本。宿主检查每条新增或改动的笔记都带有逐字出自运行记录的原话（`evidence`），没有删除笔记或历史，索引不超限。通过后替换当前记忆，旧版本保留在 `data/memory/versions/`。检查失败则本次作废；改动过大则等待确认。
  - **沉淀做法**：整理发现被纠正过的做法、反复出现的任务或验证有效的流程时，起草 skill 到 `skills-drafts/<name>/`；"以后总是/不要"类规则写成 `skills-drafts/identity/PROPOSAL.md`。草稿在批准前不会被任何普通运行看到。用 `GET /v1/drafts` 查看，`POST /v1/drafts/{name}/approve` 批准（skill 进入每次运行的 skill 目录，规则追加到 IDENTITY.md），或 `POST /v1/drafts/{name}/reject -d '{"reason":"..."}'` 驳回。
  - **启用**：配置并开启 `strong_model` 后，用下面的命令建定时器（每天 3:30）。建议启用后头一周每天看一眼 `data/memory/consolidation.jsonl`：`rejected` 多说明模型没按格式写证据，`held` 多说明一次改得太多。
  - 处理结果：`POST /v1/memory/promote`（采用待确认的整理。等待期间只要记忆有任何改动——普通运行也会写记忆——这份整理就已过期，返回 409；过期的整理会在下一次整理开始时自动丢弃，记为 `discarded`，不会一直卡住后续整理）、`/discard`（丢弃）、`/rollback`（回到上一版本，可再次回滚撤销）。`GET /v1/status` 的 `memory` 显示是否有待确认的整理（`held`）、它是否已过期（`held_stale`）和版本数；每次整理的结果记在 `data/memory/consolidation.jsonl`。把定时器的 `active` 改为 `false` 即可停用。

```sh
curl --fail-with-body -sS http://127.0.0.1:8090/v1/timers \
  -H "Authorization: Bearer $(cat .agentd/data/api-token)" -H 'Content-Type: application/json' \
  -d '{"id":"memory-consolidation","cron_expr":"30 3 * * *","active":true,"prompt":"按 skills/memory-consolidation/SKILL.md 整理记忆。"}'
```

## 知识库

把参考资料导入后，每次运行都能看到资料目录，并可用 `search` 和 `read_file` 查阅原文；资料对模型只读。

```sh
curl --fail-with-body -sS http://127.0.0.1:8090/v1/knowledge/import \
  -H "Authorization: Bearer $(cat .agentd/data/api-token)" -H 'Content-Type: application/json' \
  -d '{"path":"/path/to/docs"}'
curl -sS http://127.0.0.1:8090/v1/knowledge -H "Authorization: Bearer $(cat .agentd/data/api-token)"
```

- md、markdown、txt 直接导入。pdf、docx、pptx、xlsx、html、csv、epub 需要在配置的 `knowledge.converter` 填一个转换器（如 `["markitdown"]`），它在无网络的沙箱里运行，所需目录（如它的 Python 虚拟环境）写进 `knowledge.converter_paths`。Linux 需要可用的 bubblewrap，macOS 使用 sandbox-exec；沙箱不可用时这些格式导入失败，不会退回不隔离的转换。
- 重复导入同一目录只处理有变化的文件；文件改了会替换并归档旧版本，目录里删掉的文件会归档并移出目录。资料目录与 MEMORY.md 同样最多 200 行、8KB。单个文件转换后不超过 1MiB，更大的请拆分。

## 文档

文档按设计、现状、报告、归档分类，关系和阅读顺序见 [docs 索引](docs/README.md)。常用的几份：

- [当前软件架构：模块、数据流与状态位置](docs/current/architecture.md)
- [运行、API、权限与故障恢复](docs/current/operations.md)
- [钉钉接入步骤](docs/current/dingtalk-setup.md)（首次接入的验证记录见 [reports/2026-11-17-dingtalk-integration.md](docs/reports/2026-11-17-dingtalk-integration.md)）
- [第一阶段架构基线（已实现）](docs/design/01-stage-1-architecture.md)
- [第二阶段：记忆模块设计（部分实现）](docs/design/02-stage-2-memory.md)
- [运行状态目录与路径约定（`.agentd/` 已启用，角色分目录未实施）](docs/design/90-convention-storage-layout.md)

默认禁止任意 shell；外部写操作通过配置好的命名 HTTP 操作执行，支持人工审批和结果未知保护。完整实现范围、沙箱平台限制及设计取舍见运行文档。

## OpenCode 执行与钉钉连续对话

本机的 `config.local.yaml` 使用 `opencode_binary: ".tools/opencode"`。新部署请将适合本机平台的 OpenCode **2.2.6 原生可执行文件**放到这个位置并赋予执行权限，再在自己的配置里设置该字段。配置模板留空时使用原生 Go 执行循环。启动阶段会检查可执行文件。模型地址和模型名仍取自 `model` 配置。

同一应用、发送者、钉钉会话始终对应同一 OpenCode session。会话 ID 由这些标识确定性生成，OpenCode 自己的会话数据保存在 `data/opencode/`，备份时包含整个目录。首次切换会带入已有 Inbox 输入和已发送的 Outbox 回复，条数受 `max_history_messages` 限制。原生 Go 模式也从这些记录恢复同一钉钉会话的近期对话。agentd 的四张业务表无需迁移。不同会话共享所有者的工作区文件。

OpenCode 负责推理和工具循环，每轮可以调用内置 websearch，搜索词通过 TinyFish 查询。文件、通知、定时器及命名动作由本机临时 MCP 接口转交 agentd，沿用工作区权限、审批与动作账本。钉钉回复仍由 Outbox 投递。搜索可直接发“请联网搜索 Go 官方网站，给出来源链接”。

`GET /v1/status` 的 `executor` 显示当前执行器。如果把日志重定向到 `.agentd/data/agentd.log`，运行 `tail -f .agentd/data/agentd.log` 可查看 `opencode run` 的事件号和会话号，以及 `opencode tool` 的工具名和结果状态。真实模型和搜索测试使用临时工作区，可运行 `AGENTD_OC_LIVE=1 ./scripts/go test -v ./internal/agent -run '^TestLiveOpenCodeChat$' -count=1`。

## 钉钉 AI 资讯定时私聊

任务 `ai-news-private` 每个整点检查一次新闻源，将新增文章私聊发送给 `dingtalk.allowed_sender`。每轮按发布时间从新到旧推送全部新增，超出钉钉单条消息长度时自动分条，无新增时静默。相同原文链接只推一次（忽略 URL 片段和 UTM 跟踪参数），不同媒体对同一事件的报道可分别出现。

订阅清单位于 `config.local.yaml` 的 `news_feeds`，与配置模板格式一致。当前配置覆盖量子位、TechCrunch AI、Google AI、OpenAI、Anthropic、Hugging Face、MIT News AI、AWS 机器学习、NVIDIA 生成式 AI、MIT 科技评论 AI 和法国《世界报》AI 版。增删订阅源后重启 agentd，每个源填写名称、可直接访问的 HTTPS RSS 2.0 地址和语言（如 `zh`、`en`、`fr`）。Anthropic 单独读取官方 Newsroom 列表，按官网展示发布日期。钉钉显示中文标题、来源、发布时间和原文链接。外语标题去重后由已配置的本地模型翻成简体中文，原文链接仍指向发布者的原语言文章。

新闻源、翻译或推送逻辑变更部署后，维护者立即向订阅者私聊实发一条测试消息，并核对钉钉受理结果。运行 `./scripts/news-push-test Anthropic add-anthropic-newsroom`，将两个参数替换成实际源名和本次变更标识。脚本使用线上实例抓取该源最近一篇文章，走正常翻译与 Outbox 投递路径，并显示投递编号。`sent` 表示钉钉 API 已受理。同一次变更重跑时沿用原标识，可查询同一条消息。测试消息带有“新闻推送测试”标记，每小时的增量任务继续按原计划执行。

首次启用从第一个定时任务触发前一小时开始取新增，已有订阅沿用原来的增量起点。某个源暂时不可用时，其他源照常处理。翻译没有获得有效中文的条目不计入已推送，后续轮询仍能从 RSS 读到时会再尝试。定时私聊依赖 agentd 运行、网络和机器人主动单聊权限。

在项目目录使用本机管理接口启用、修改频率或关闭。下面的命令为每小时整点一次，将 `active` 改成 `false` 即可停止后续触发。

```sh
curl --fail-with-body -sS http://127.0.0.1:8090/v1/timers \
  -H "Authorization: Bearer $(cat .agentd/data/api-token)" -H 'Content-Type: application/json' \
  -d '{"id":"ai-news-private","cron_expr":"0 * * * *","active":true,"prompt":"AI 资讯私聊"}'
```

## 企业微信团队模式

在 `config.local.yaml` 设置 `im.provider: wecom` 并重启，即选择企业微信智能机器人长连接。设置为 `dingtalk` 可切回；省略时沿用钉钉及其 `enabled` 配置。一次只启动一个渠道连接，两端共用本机 SQLite、Inbox、OpenCode、本地模型、Outbox 和新闻定时器。旧渠道的待投递消息保留原目标。

1. 在企业微信创建 **智能机器人 → API 模式 → 长连接**，从后台取得 Bot ID、Secret，并将机器人加入试点内部群。agentd 主动连接企微，不需要公网回调入口。[企微官方长连接协议](https://developer.work.weixin.qq.com/document/path/101463)
2. 从 [配置模板](config.yaml) 复制 `im` 和 `wecom` 两段到本地配置。填入 `bot_id`、`secret`。若还不知道用户或群 ID，先打开 `discovery_only`，重启后给机器人发私聊并在群里 @ 一次。agentd 日志中的 `wecom discovery` 会显示回调的 `userid`、`chatid`。发现模式只记录标识。
3. 将本人回调的 `userid` 写入 `owner_userid`，将 `wecom.allow_all_groups` 设为 `true` 后，机器人加入的每个内部群都会自动开放给该群所有成员 @ 提问，新群无需登记或重启。若只开放指定群，设为 `false` 并将群 ID 写入 `allowed_groups`。在 `team_knowledge` 中填写数字员工的角色和可公开给这些群的团队资料（支持 YAML 多行文本）。设置 `discovery_only: false` 后重启。`GET /v1/status` 中检查 `im_provider` 和 `wecom.connected`。
4. 所有者在私聊中可以使用个人工作区及已授权工具。启用群内的任何成员均可 @ 机器人提问，每位成员在每个群的对话历史独立。默认群聊只使用团队资料和对话历史。显式设置 `wecom.group_owner_tools: true` 并重启后，所有获准群的所有成员都可调用所有者私聊可用的工具，包括 OpenCode 联网搜索和个人工作区读写（例如 `MEMORY.md` 和 `notes/`），结果会发回原群。全局工具策略、文件写入范围和 IM 渠道限制仍然适用。普通未 @ 的群消息是否可见取决于企微回调；此接入处理机器人收到的文本和文本引用。
5. 需要提醒固定负责人的群可配置 `group_leads: {"群chatid": "负责人userid"}`。原群的结果消息会带指定成员的提及标记，处理中的进度消息不提醒。所有者也可以在群里 @ 机器人后发送 `/提醒 bob 总结今天的问题`，为该任务明确指定 userid。模型生成的提及标记按普通文字处理。用第二位成员的企微客户端验收是否出现实际 @ 提醒。
6. 小时新闻发到当前渠道所有者的**私聊**。先由接收人在企微与机器人发起一次会话，再运行 `./scripts/news-push-test Anthropic wecom-first-run`。检查 Outbox 的 `sent` 和手机收到的中文测试消息。对同一渠道同一次测试重跑使用相同标识。企微长连接发送成功回执表示平台受理；发送途中断连记为 `unknown`，由管理员核对。
