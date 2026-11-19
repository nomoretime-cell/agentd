# agentd

一个使用 Go 实现的单机 Always-on Agent，保留 Event Inbox、Timers、Memory、Permissions、Outbox 五个组件。核心采用一个业务包、SQLite 四张表和六个基础工具。

## 启动

```sh
./scripts/bootstrap
make build
./bin/agentd -config config.yaml -root .
```

- 服务：`http://127.0.0.1:8090`，健康检查 `/healthz`。
- 模型：`http://localhost:8080/v1/chat/completions`，模型名称见 `config.yaml`。
- API 密钥：首次启动生成 `data/api-token`，除健康检查外均需 Bearer Token。
- 默认投递：保存到 `data/deliveries/`；普通通知遵循配置中的夜间静默时段。
- Go 工具链、依赖、构建缓存和测试数据均保存在项目 `.tools/`、`.cache/` 中。

```sh
TOKEN=$(cat data/api-token)
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

## 文档

- [钉钉接入、验证结果与真实会话联调](docs/dingtalk-validation-codex.md)
- [运行、API、权限与故障恢复](docs/implementation-codex.md)
- [本次实现依据的最简设计](docs/system-architecture-minimal-gemini-latest.md)
- [原始文章架构解析](docs/README.md)

默认禁止任意 shell；外部写操作通过配置好的命名 HTTP 操作执行，支持人工审批和结果未知保护。完整实现范围、沙箱平台限制及设计取舍见运行文档。

## OpenCode 执行与钉钉连续对话

本机的 `config.local.yaml` 使用 `opencode_binary: ".tools/opencode"`。新部署请将适合本机平台的 OpenCode **2.2.6 原生可执行文件**放到这个位置并赋予执行权限，再在自己的配置里设置该字段。配置模板留空时使用原生 Go 执行循环。启动阶段会检查可执行文件。模型地址和模型名仍取自 `model` 配置。

同一应用、发送者、钉钉会话始终对应同一 OpenCode session。会话 ID 由这些标识确定性生成，OpenCode 自己的会话数据保存在 `data/opencode/`，备份时包含整个目录。首次切换会带入已有 Inbox 输入和已发送的 Outbox 回复，条数受 `max_history_messages` 限制。原生 Go 模式也从这些记录恢复同一钉钉会话的近期对话。agentd 的四张业务表无需迁移。不同会话共享所有者的工作区文件。

OpenCode 负责推理和工具循环，每轮可以调用内置 websearch，搜索词通过 TinyFish 查询。文件、通知、定时器及命名动作由本机临时 MCP 接口转交 agentd，沿用工作区权限、审批与动作账本。钉钉回复仍由 Outbox 投递。搜索可直接发“请联网搜索 Go 官方网站，给出来源链接”。

`GET /v1/status` 的 `executor` 显示当前执行器。运行 `tail -f data/agentd.log` 可查看 `opencode run` 的事件号和会话号，以及 `opencode tool` 的工具名和结果状态。真实模型和搜索测试使用临时工作区，可运行 `AGENTD_OC_LIVE=1 ./scripts/go test -v ./internal/agent -run '^TestLiveOpenCodeChat$' -count=1`。

## 钉钉 AI 资讯定时私聊

内置任务 ID 为 `ai-news-private`。它按发布时间选取可访问的 TechCrunch AI、Google AI 和量子位 RSS 中的新增文章，通过钉钉应用主动单聊接口发送给 `dingtalk.allowed_sender`。每个整点检查一次，有多少新增就推送多少，按发布时间从新到旧排列。超出单条消息长度时分条发送，无新增时保持静默。去重依据为同一接收人的历史投递链接，忽略 URL 片段和 UTM 跟踪参数。首次简报之前发布的旧文章不会补发。所有来源均不可用时跳过该轮。这个订阅按设定时间全天投递，依赖 agentd 运行、网络和机器人的主动单聊权限。

在项目目录使用本机管理接口启用、修改频率或关闭。下面的命令为每小时整点一次，将 `active` 改成 `false` 即可停止后续触发。

```sh
curl --fail-with-body -sS http://127.0.0.1:8090/v1/timers \
  -H "Authorization: Bearer $(cat data/api-token)" -H 'Content-Type: application/json' \
  -d '{"id":"ai-news-private","cron_expr":"0 * * * *","active":true,"prompt":"AI 资讯私聊"}'
```
