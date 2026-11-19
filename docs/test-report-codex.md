# agentd 本地验证报告

验证日期：2026-11-17（Asia/Shanghai）。平台：macOS arm64。工具链：项目内 Go 1.27.4；SQLite 驱动：modernc.org/sqlite v1.62.0。

**验证结果**

| 检查 | 结果 |
| --- | --- |
| 23 个自动化测试，真实 SQLite、HTTP 测试接收器 | 通过 |
| `CGO_ENABLED=1 ./scripts/go test -race ./... -count=1` | 通过，无报告的数据竞争 |
| `./scripts/go vet ./...` | 通过 |
| `CGO_ENABLED=0 ./scripts/go build -trimpath -o bin/agentd ./cmd/agentd` | 通过，生成 macOS arm64 二进制 |
| `./scripts/go mod verify` | 所有模块通过校验 |
| `govulncheck`（本次运行使用 golang.org/x/vuln v1.9.0） | No vulnerabilities found |
| 真实本地模型端到端测试 | 通过，约 24 秒 |
| 独立二进制启动、真实推理、投递及重启测试 | 通过 |

普通测试覆盖：固定窗口与并发去重、队头阻塞、定时器单次与周期推进、审批前禁止执行、批准后一次执行、动作重放与参数冲突、模型重排工具后不重复写、未知结果不重发、拒绝/过期、事务回滚、发送失败与退避、暂停取消不增加故障计数、重启恢复、文件版本冲突、路径穿越、凭证隔离、沙箱网络禁止和子进程超时。

**真实模型验证**

- 地址：`http://localhost:8080/v1/chat/completions`
- 模型：`/Users/yejibing/models/Qwen3.8-27B-4bit`
- 验证链路：HTTP 输入 → 模型原生工具调用 → 写笔记 → Outbox 本地投递；后续回合读取相同笔记；持久化定时器触发 → 模型通知；审批挂起 → 人工批准 API → loopback 模拟远端写一次 → 模型读取回执并通知。
- 所有远端写验证只指向测试期间创建的本机 HTTP 接收器，没有发送真实业务消息。

详细证据：[真实模型报告](../.cache/live-e2e/20261116T165506.952229000/report.json)、[二进制报告](../.cache/binary-e2e/20261116T165515922207/report.json)。这些运行产物位于被 Git 忽略的项目缓存中；清理缓存后可使用 `make live` 和 `python3 scripts/smoke.py` 重新生成。

测试验证当前平台和当前模型的这些路径，不代表所有第三方 API 均已接入或能够承诺外部副作用恰好一次。Linux bubblewrap 分支未在本次 macOS 环境实机验证；原生 IM 渠道需后续适配，当前提供本地投递和通用 Webhook。
