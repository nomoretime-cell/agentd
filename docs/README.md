# agentd 文档索引

第一次看项目先读 [当前软件架构](current/architecture.md)。

文档按用途分四类：**设计**（design/）说明系统应该是什么样，**现状**（current/）说明代码现在怎样、怎么运行，**报告**记录某一时点的验证结果，**归档**是已被取代的早期草稿。

## 目录

| 目录 | 内容 | 维护方式 |
| --- | --- | --- |
| [design/](design/) | 背景、各阶段架构设计、跨阶段约定 | 文件名按 `<序号>-<类别>-<主题>.md`，序号即阅读顺序：`00` 背景资料，`01`–`89` 按阶段顺序的设计（`stage-<N>`），`90` 起为跨阶段约定。每份开头标注状态（已实现 / 部分实现 / 待实现）；设计变化时更新 |
| [current/](current/) | 当前架构、运行与运维、渠道接入 | 描述当前代码的行为，随代码同步更新；验证记录不放这里，放 reports/ |
| [reports/](reports/) | 验证报告、性能与选型测试 | 文件名带日期，写完不再修改；新的验证另写一份 |
| [archive/](archive/) | 第一阶段之前的设计草稿 | 冻结，只作历史参考 |

## 设计文档之间的关系

```text
00-background-always-on-agents.md   文章解析：五大原语的出处
        │  提炼为
        ▼
01-stage-1-architecture.md          第一阶段：五大原语的最简实现（已实现）
        │  替换其中的 Memory 组件
        ▼
02-stage-2-memory.md                第二阶段：记忆模块（部分实现）

90-convention-storage-layout.md     跨阶段约定：运行状态目录与路径（`.agentd/` 已启用，角色分目录未实施）
                                    被第二阶段及之后的设计引用
91-convention-roadmap.md            跨阶段约定：开发路线，把各设计文档的后续步骤排成一条顺序

archive/                            第一阶段之前的草稿，stage-1 由它们收敛而来
```

## 文档列表

| 文档 | 用途 | 状态 |
| --- | --- | --- |
| [design/00-background-always-on-agents.md](design/00-background-always-on-agents.md) | Anthropic《Building always-on agents》解析，五大原语的出处 | 背景资料 |
| [design/01-stage-1-architecture.md](design/01-stage-1-architecture.md) | 第一阶段架构基线：1 个进程、5 个组件、4 张表、6 个工具 | 已实现 |
| [design/02-stage-2-memory.md](design/02-stage-2-memory.md) | 第二阶段记忆模块：四类数据、三个机制、关键动作与模型分级、作用域 | 第 1–4 步已实现 |
| [design/90-convention-storage-layout.md](design/90-convention-storage-layout.md) | 运行状态目录 `.agentd/`、虚拟路径、作用域隔离、路径开发约定 | `.agentd/` 已启用，角色分目录未实施 |
| [design/91-convention-roadmap.md](design/91-convention-roadmap.md) | 开发路线：现状、阶段 A–D 的顺序与依赖、完成标准、待决定事项 | 阶段 A、B 已完成，待运行验证 |
| [current/architecture.md](current/architecture.md) | 当前软件架构：模块与文件、4 张表、事件/审批/记忆数据流、运行状态位置 | 随代码更新 |
| [current/operations.md](current/operations.md) | 启动、HTTP 接口、工具与权限、审批与故障恢复、运维 | 随代码更新 |
| [current/dingtalk-setup.md](current/dingtalk-setup.md) | 钉钉执行路径、接入步骤、复现测试与当前边界 | 随代码更新 |
| [reports/2026-11-17-dingtalk-integration.md](reports/2026-11-17-dingtalk-integration.md) | 钉钉首次接入的验证记录：已验证行为与真实本地模型端到端测试 | 2026-11-17，冻结 |
| [reports/2026-11-17-local-validation.md](reports/2026-11-17-local-validation.md) | 第一阶段实现后的本地验证报告 | 2026-11-17，冻结 |
| [reports/2026-11-22-inkling-2-mini-vs-qwen38-dflash.md](reports/2026-11-22-inkling-2-mini-vs-qwen38-dflash.md) | 本地模型选型：Inkling 2 Mini 4-bit 与 Qwen3.8-27B + DFlash 2 的速度与可用性对比 | 2026-11-22，冻结 |

企业微信、新闻推送和 OpenCode 的用法写在仓库根目录的 [README](../README.md)；开发约定写在 [AGENTS.md](../AGENTS.md)。

## 冲突时以谁为准

- **现在的行为**：代码与测试 > current/ > README。
- **应该怎么设计**：AGENTS.md 的架构不变量 > design/ > archive/。
- design/ 中标注"待实现"的内容不代表代码已经具备；开发前先看文档开头的状态。
