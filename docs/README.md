# agentd 文档索引

文档按用途分四类：**设计**说明系统应该是什么样，**指南**说明现在怎么运行，**报告**记录某一时点的验证结果，**归档**是已被取代的早期草稿。

## 目录

| 目录 | 内容 | 维护方式 |
| --- | --- | --- |
| [design/](design/) | 背景、各阶段架构设计、跨阶段约定 | 每份开头标注状态（已实现 / 部分实现 / 待实现）；设计变化时更新 |
| [guides/](guides/) | 运行、接口、渠道接入 | 描述当前代码的行为，随代码同步更新 |
| [reports/](reports/) | 验证报告 | 文件名带日期，写完不再修改；新的验证另写一份 |
| [archive/](archive/) | 第一阶段之前的设计草稿 | 冻结，只作历史参考 |

## 设计文档之间的关系

```text
background-always-on-agents.md        文章解析：五大原语的出处
        │  提炼为
        ▼
stage-1-architecture-baseline.md      第一阶段：五大原语的最简实现（已实现）
        │  替换其中的 Memory 组件
        ▼
stage-2-memory-design.md              第二阶段：记忆模块（部分实现）

storage-layout.md                     跨阶段约定：运行状态目录与路径（目标布局，迁移未实施）
                                      被第二阶段及之后的设计引用

archive/                              第一阶段之前的草稿，stage-1 由它们收敛而来
```

## 文档列表

| 文档 | 用途 | 状态 |
| --- | --- | --- |
| [design/background-always-on-agents.md](design/background-always-on-agents.md) | Anthropic《Building always-on agents》解析，五大原语的出处 | 背景资料 |
| [design/stage-1-architecture-baseline.md](design/stage-1-architecture-baseline.md) | 第一阶段架构基线：1 个进程、5 个组件、4 张表、6 个工具 | 已实现 |
| [design/stage-2-memory-design.md](design/stage-2-memory-design.md) | 第二阶段记忆模块：四类数据、三个机制、关键动作与模型分级、作用域 | 部分实现 |
| [design/storage-layout.md](design/storage-layout.md) | 运行状态目录 `.agentd/`、虚拟路径、作用域隔离、路径开发约定 | 目标已定，迁移未实施 |
| [guides/runtime.md](guides/runtime.md) | 启动、HTTP 接口、工具与权限、审批与故障恢复、运维 | 随代码更新 |
| [guides/dingtalk.md](guides/dingtalk.md) | 钉钉接入步骤与首次接入时的验证记录 | 接入步骤随代码更新 |
| [reports/2026-11-17-local-validation.md](reports/2026-11-17-local-validation.md) | 第一阶段实现后的本地验证报告 | 2026-11-17，冻结 |

企业微信、新闻推送和 OpenCode 的用法写在仓库根目录的 [README](../README.md)；开发约定写在 [AGENTS.md](../AGENTS.md)。

## 冲突时以谁为准

- **现在的行为**：代码与测试 > guides > README。
- **应该怎么设计**：AGENTS.md 的架构不变量 > design/ > archive/。
- design/ 中标注"待实现"的内容不代表代码已经具备；开发前先看文档开头的状态。
