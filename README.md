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
