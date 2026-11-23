# agentd 第二阶段：记忆模块设计（部分实现）

2026-11-23 · 在线版本：https://claude.ai/artifact/LJaZjYaeUwyB6RXt674s4V

> **文档状态：第二阶段设计，演进路线的第 1、2 步已实现（2026-11），其余仍是草案。** 本文以[第一阶段架构基线](stage-1-architecture-baseline.md)为前提，只替换其中的 Memory 组件，其余四个组件（Inbox、Timers、Permissions、Outbox）保持不变。落地方式见文末[在 agentd 中的落地](#在-agentd-中的落地)。
>
> - **取代基线的部分**：`session.jsonl` 从"可丢弃的滑动缓存"改为"只追加的完整原始记录"（`sessions/`）；目录结构从 `workspace/notes` 改为 `memory/`、`skills/`、`skills-drafts/`、`knowledge/`；新增"整理"（由 Timers 触发的一次普通会话），不新增后台循环。
> - **与基线预算的冲突，需要单独决策**：用 SQLite FTS5 实现 `search` 会新增一张表，超出"4 张表"的预算。第一步改用文件检索（grep），可以不新增表。

## 设计目标与原则

记忆模块由四类数据和三个机制组成：原始数据（session、knowledge）只读且永久保留，提炼数据（memory、skills）是可随时重建的缓存。目标是让 agent 跨会话越用越好，同时在模型能力大幅提升时架构不失效。

五条原则，均从"模型无状态，记忆 = 未来会话的上下文里出现什么"推出：

1. **事实与派生分离。** 原始记录只追加、不修改；notes、skills、索引都是派生物。更强的模型可以从原始数据重新提炼，旧模型的错误不会永久固化（event sourcing 思路）。
2. **代码管不变量，模型管判断。** 代码负责只追加、容量上限、原子写、来源追溯、归档不删除、变更审核；记什么、怎么组织、何时沉淀为 skill 由模型决定。模型变强时只删 prompt 指导，不改代码。
3. **文件是唯一接口。** 所有记忆都是模型能用通用工具（read / write / search）直接读写的纯文本，不存在模型看不见的记忆库。
4. **加载层小而固定，按需层可增长。** 每次会话只加载有硬上限的索引与目录，详细内容按需读取。
5. **离线整理为主，在线写入为辅。** 在线写入会分散注意力且不知道任务结局；离线整理能看到完整结果，可对照原始数据校验。

不做的事：固定抽取 schema、数字触发写进 prompt、只靠向量检索、独立的抽取管线、细粒度记忆分类体系。

## 总体架构

```text
        原始层 · agent 只读            机制                    提炼层 · 可从原始层重建
   ┌──────────────────────┐                              ┌──────────────────────┐
   │ session              │  W4 读  ┌──────────────┐ 更新 │ memory               │
   │ 完整会话记录·只追加  │───────▶│ 整理 Dreaming │─────▶│ 什么是真的·陈述性    │
   │                      │    ┌──▶│ 校验·提炼·合并 │      │                      │
   │ knowledge            │────┘   └──────┬───────┘      │ skills               │
   │ 导入资料·W2 导入器写 │               │ skill 草稿    │ 怎么做·程序性        │
   └──────────▲───────────┘        ┌──────▼───────┐ W5   │                      │
              │                    │ 闸门          │─────▶│                      │
              │                    │ 草稿审核·回滚 │ 生效  └──────────┬───────────┘
              │                    └──────────────┘                 │ 索引 + 目录
              │                    ┌──────────────┐                 │
              │                    │ 装配          │◀────────────────┘
              │                    │ 身份+索引+目录 │
              │                    └──────┬───────┘
              │ W1 记录 / R3 检索          │ R1 会话开始加载
              │                    ┌──────▼───────┐   W3 在线写 / R2 按需读
              └───────────────────▶│ 会话 agent loop│◀ ─ ─ ─ ─ ─ ─ ─ ─ ─ ▶ 提炼层
                                   └──────────────┘
```

左侧原始层由 harness 和导入器写入，模型只读；中间三个机制把原始数据变成右侧提炼层，再经装配进入会话。虚线是会话中的少量在线写入与按需读取；W/R 编号对应「数据流」一节。

## 模块定义与边界

每类数据只有一个写入者，这是边界清晰的关键。模型对原始数据永远只读。

### 四类数据

| 模块 | 性质 | 存什么 | 唯一写入者 | 读取方式 | 明确不做 |
| --- | --- | --- | --- | --- | --- |
| session | 原始·只追加 | 每次会话的完整记录：消息、工具调用与结果 | harness（自动） | 检索（全文搜索、按会话翻阅），从不整体加载 | 不摘要、不修改、不删除 |
| knowledge | 原始·外部维护 | 导入的外部资料：原文件 + 转换后的 Markdown | 导入器（人触发） | 索引 + 检索 + 按需读原文 | 不存 agent 的理解或摘要；版本由 hash 区分 |
| memory | 提炼·陈述性 | 什么是真的：用户偏好、环境事实、决策、验证过的规则、对 knowledge 的提炼（wiki） | 模型（在线少量 + 离线整理） | 索引加载，正文按需读 | 不存能从原始数据直接查到的内容、不存流程 |
| skills | 提炼·程序性 | 某一类任务怎么做：步骤、坑、验证方法、脚本 | 模型起草 → 闸门生效 | 目录加载，正文按需读 | 不为一次性任务建 skill、不授予权限 |

归类判断：一条信息被多种任务用到 → memory；它是某个流程里的一步或一个坑 → 对应 skill 的 Pitfalls。从 knowledge 提炼的摘要和 wiki 放 memory，knowledge 始终保持原样。

### 三个机制

| 机制 | 职责 | 输入 → 输出 | 实现形态 |
| --- | --- | --- | --- |
| 装配 | 会话开始时拼出冻结的上下文前缀 | 身份 + memory 索引 + skills 目录 + knowledge 索引 → system prompt | 确定性代码，无模型参与 |
| 整理（Dreaming） | 从原始数据提炼、校验、合并、归档 | session（上次进度之后）+ knowledge + 现有 memory/skills → 更新后的 memory、skill 草稿 | 一次普通的 agent 会话，只是任务 prompt 不同；由定时器或会话结束触发 |
| 闸门 | 改变未来行为的写入需审核，可回滚 | skill 草稿、身份变更 → 生效或驳回 | 草稿区 + 审核策略 + git 版本 |

在线写入不是第四个机制：它只是模型在普通会话里用文件工具写 memory，不需要专门的记忆工具。

## 数据格式与目录结构

全部用纯文本：JSONL 存 session，Markdown + YAML frontmatter 存其余。frontmatter 只放代码需要读的字段，正文由模型自由组织。

```text
root/
├── IDENTITY.md                 # 身份与规则，人维护，变更走闸门
├── sessions/                   # 原始·只追加
│   └── 2026-11-23/<session_id>.jsonl
├── knowledge/                  # 原始·外部维护，agent 只读
│   ├── INDEX.md                # 代码生成：一行一个文档
│   ├── raw/<doc_id>.<ext>      # 原文件
│   └── md/<doc_id>.md          # 转换后的 Markdown
├── memory/                     # 提炼·模型可写
│   ├── MEMORY.md               # 模型维护的索引，代码限上限
│   ├── <topic>.md              # 一个主题一个文件
│   └── .archive/               # 归档，不进索引
├── skills/                     # 提炼·已生效，模型只读
│   ├── <name>/SKILL.md (+ references/ scripts/)
│   └── .archive/
├── skills-drafts/              # 模型可写，等待闸门
└── state/
    ├── usage.jsonl             # 读取统计（harness 写）
    └── consolidation.json      # 整理进度
```

整个 root（除 sessions/ 外）用 git 管理，每次整理一个提交，审计与回滚都靠它。

### session 记录

一行一条事件，只追加：

```json
{"ts":"2026-11-23T10:02:11+08:00","session":"s_20261123_a1","seq":17,"role":"tool","tool":"read_file","args":{"path":"memory/deploy.md"},"content":"..."}
```

必须包含工具调用与结果：整理时靠它判断任务是否成功、哪条 memory 是错的。

### memory 文件

```markdown
---
description: 用户偏好中文简洁回复，技术术语保留英文   # 一句话摘要，供检索和写索引行参考
status: verified          # verified | unconfirmed
sources: [s_20261120_b3, knowledge:doc_7f2a]   # 来源，供整理校验
valid_until: 2027-03-31   # 可选，有时效的事实
updated: 2026-11-23
---
正文自由组织。可用 [[other-topic]] 链接其他条目。
```

### skill 文件（兼容 Agent Skills / agentskills.io）

```markdown
---
name: weekly-report            # 小写+连字符，按一类任务命名
description: 生成周报。当用户要汇总本周工作或进展时使用。   # 做什么 + 何时用
version: 3
sources: [s_20261102_c1, s_20261116_a4]
---
## When to Use
## Procedure
## Pitfalls      # 一条规则 + 一句原因，不写日期和工单号
## Verification  # 怎么确认做对了
```

### knowledge 元数据

每个文档在 INDEX.md 对应一行，另存一份机器可读清单：`doc_id`、来源路径、sha256、mtime、转换器与版本、导入时间。再次导入时 hash 不变就跳过；源文件删除则移入归档。支持格式：md、txt 原生；pdf、docx、pptx、xlsx、html、csv、epub 经 MarkItDown 或 Docling 转为 Markdown。

## 数据流

数据只有一个方向：原始 → 提炼 → 加载。提炼数据从不反向写回原始层，所以原始层可以永远当作校验基准。

### 写入路径

| 路径 | 触发 | 写入者 | 从 → 到 | 约束 |
| --- | --- | --- | --- | --- |
| W1 记录 | 每条消息、每次工具调用 | harness | 会话 → sessions/ | 只追加，模型不可写 |
| W2 导入 | 人执行导入命令、上传或放入监视目录 | 导入器 | 外部文件 → knowledge/ | hash 去重，转换在沙箱内执行 |
| W3 在线记忆 | 用户明确要求记住，或模型判断明显值得记 | 模型（会话中） | 会话 → memory/（主题文件 + MEMORY.md 中对应的行） | 先读后写（版本冲突检测）；下次会话才进入前缀 |
| W4 整理 | 定时器或会话结束 | 模型（整理会话） | sessions/ + knowledge/ → memory/（含 MEMORY.md）+ skills-drafts/ | 从上次进度继续；每次一个 git 提交 |
| W5 生效 | 审核通过 | 闸门 | skills-drafts/ → skills/ | 可回滚；驳回理由写入 memory 供下次参考 |
| W6 统计 | 每次读取 memory 或 skill | harness | 工具调用 → state/usage.jsonl | 供归档使用 |
| W7 索引 | W2、W5 之后 | 代码 | frontmatter → skills 目录、knowledge/INDEX.md | 超上限则拒绝触发它的那次导入或生效，并返回当前条目 |

MEMORY.md 不由 W7 生成，做法与 Claude Code 一致：模型在 W3、W4 中和主题文件一起维护索引，代码只管上限。`write`/`edit` 写 MEMORY.md 时校验行数和字节数（如 200 行），超限整次拒绝并返回当前内容，模型必须先合并；装配时再按同一上限截断，兜住人直接编辑文件的情况。这样分组和措辞由模型决定，代码不必解析 frontmatter 拼索引。代价是索引可能和主题文件不同步（漏加一行，或留下指向已归档文件的行），由整理（W4）顺带校对。

### 整理（W4）的任务内容

整理会话的 prompt 只说明目的和不变量，不规定具体做法。建议涵盖五件事：

1. **校验**：对照新增的 session 检查已有 memory，改正错误，把被证实的条目标为 verified。
2. **提炼**：只记"看原文也看不出来"的东西：验证过的规则、计划、偏好、坑。
3. **合并与替换**：相似条目合成一条；与新事实矛盾的旧条目直接覆盖。
4. **skill 草稿**：用户纠正过做法、同类任务重复出现、一个不简单的流程被验证有效时起草。优先修改已有 skill，最后才新建；修改前必须先读。不记环境特有的失败和偶发错误。
5. **归档与校对索引**：按使用统计归档长期未用的条目，处理过期的 valid_until，并删去 MEMORY.md 中对应的行；顺带核对 MEMORY.md 与主题文件是否一一对应。

### 读取路径

| 路径 | 时机 | 内容 | 方式 |
| --- | --- | --- | --- |
| R1 装配 | 会话开始 | 身份 + MEMORY.md + skills 目录（name + description）+ knowledge/INDEX.md | 拼进 system prompt，整场会话内冻结；会变的内容（当前时间）放末尾，保住前缀缓存 |
| R2 按需读 | 会话中 | memory 正文、SKILL.md 及其 references/、knowledge 原文 | 通用 read 工具 |
| R3 检索 | 会话中 | sessions/ 与 knowledge/ | search 工具：先全文（FTS5 + trigram 分词以支持中文），需要时加向量并用 RRF 融合 |

prompt 里只需说明记忆的目的："memory/ 存在是为了让你更好地完成 {目标}，由你决定记什么"，再加一句何时该查 memory 和搜 session。较弱的模型不提示就不读记忆，这句不能省。

## 跨会话生命周期

一次会话学到的东西，经过整理后在下一次会话开始时生效。一个完整周期：

1. **会话 N 开始**：装配拼出冻结前缀（R1）。
2. **会话中**：模型按需读取和检索（R2、R3）；harness 持续记录（W1）并统计使用（W6）；在线记忆（W3）立即落盘，工具结果中可见，但不改当前前缀。
3. **会话 N 结束或定时触发**：整理会话（W4）从上次进度读到当前，更新 memory 与 skill 草稿，索引重建（W7），提交 git。
4. **闸门**：草稿审核后生效（W5）。
5. **会话 N+1 开始**：拿到更新后的索引与 skill 目录。

### 会话边界

always-on agent 没有天然的会话边界，需要人为切分，否则冻结前缀永不刷新、整理永不触发。可选切分点：每天固定时刻、对话闲置超过一定时间、一个任务完成、上下文压缩（compaction）时。同一用户的连续对话可以跨边界带上最近几轮原文，但前缀重新装配。

### 模型升级

换成更强的模型后，可以做一次"重建"：对全部 sessions/ 和 knowledge/ 重新跑整理，生成新的 memory 与 skill 草稿，和现有版本对比后切换。同时删掉 prompt 里为旧模型加的额外指导。因为原始层从未被改动，重建没有信息损失；因为整理只是一次普通会话，重建不需要改代码。

## 增长控制与遗忘

有害的是加载层变大，不是磁盘总量变大。加载层用代码硬封顶，提炼层靠整理保持稳定，原始层允许持续增长。

| 层 | 增长是否可接受 | 控制手段 |
| --- | --- | --- |
| 加载层（MEMORY.md、skills 目录、knowledge 索引） | 不可接受 | 代码硬上限（如索引 200 行、skills 目录 50 条）。超限时写入报错并返回当前条目，模型必须先合并。不靠 prompt 里的"请保持精简" |
| memory 正文 | 适度可接受 | 合并相似条目、矛盾即覆盖、valid_until 到期失效、长期未读归档、能从原文查到的不记 |
| skills 正文 | 适度可接受 | 优先修改已有 skill；重叠的合并为大类；14 天未用标 stale、30 天归档（Hermes 默认值）；被定时任务引用的或人工固定的豁免 |
| sessions/、knowledge/ | 可接受 | 只检索不加载；旧数据可压缩或转冷存储 |

两条规则：

- **只归档不删除。** 归档条目移出索引但仍可检索。因为原始层永远保留，归档错了也能重新提炼，所以提炼层可以大胆删减。
- **监控撞上限的频率。** 频繁撞限说明记得太细（提高抽象层次），或作用域该拆分（见下一节）。

## 作用域、权限与闸门

### 作用域

作用域只是 memory 与 sessions 的一个目录维度，不增加模块。装配按当前会话的身份选择加载哪些目录。单用户部署不分作用域，即上一节的平铺结构；分作用域后，每个作用域目录各有自己的 MEMORY.md 与 .archive/，各自计算上限。

| 作用域 | 目录 | 谁能读 | 谁能写 |
| --- | --- | --- | --- |
| 全局 | memory/global/ | 所有会话 | 仅整理会话 |
| 个人 | memory/users/<user_id>/ | 该用户的会话 | 该用户的会话 + 整理 |
| 群组 | memory/groups/<group_id>/ | 该群的会话 | 该群的会话 + 整理 |

skills 和 knowledge 默认全局共享；敏感资料可按同样方式分目录。整理时把个人或群组信息提升到全局，必须走闸门，防止跨用户泄漏。

### 权限

权限按路径定义，由工具层执行，不靠 prompt。

| 路径 | 模型读 | 模型写 |
| --- | --- | --- |
| IDENTITY.md | 可 | 否（只能提议，走闸门） |
| sessions/、knowledge/ | 可（检索） | 否 |
| memory/（含 MEMORY.md） | 可 | 可（版本冲突检测；MEMORY.md 另有上限校验） |
| skills/ | 可 | 否 |
| skills-drafts/ | 可 | 可 |
| skills 目录、knowledge/INDEX.md、state/ | 可 | 否（代码生成） |

所有进入 memory 和 skill 的内容在写入时检查提示注入与外泄特征（Hermes 做法）；来自 session 和 knowledge 的文字一律当作数据，不当作指令。skill 只描述怎么做，不能授予权限。

### 闸门策略

闸门的严格程度是一个配置项，随模型能力调整，架构不变：

- **manual**：所有 skill 草稿人工审核。适合本地中小模型。
- **verify**：草稿附带 Verification，用它在沙箱里重跑一次来源任务，通过即生效，人事后抽查。
- **auto**：直接生效，仅保留 git 回滚。仅在模型可靠、影响面可控时使用。

## 工具接口清单

模型只用四个通用工具，没有专门的 memory 或 skill 工具。所有约束由工具层按路径执行。

| 工具 | 参数 | 记忆相关行为 |
| --- | --- | --- |
| `read` | path, range? | 读 memory、skill、knowledge 原文；返回内容 hash 供后续写入；记录使用统计（W6） |
| `write` | path, content, expected_hash | 全量写；仅允许 memory/ 与 skills-drafts/；hash 不符则拒绝（先读后写）；校验 frontmatter 必填字段；写 MEMORY.md 时校验上限，超限整次拒绝并返回当前内容 |
| `edit` | path, old, new, expected_hash | 局部替换，约束同 `write`；patch skill 草稿时优先使用 |
| `search` | query, scope (sessions / knowledge / memory), filters? | 全文检索返回片段与定位（会话 id + seq，或文档 id + 标题路径），不做 LLM 摘要 |

模型不直接删除或移动文件。归档时在 frontmatter 写 `archived: true` 并从 MEMORY.md 删去对应行，由代码移入 .archive/。

非模型接口（给人和定时器）：

| 接口 | 作用 |
| --- | --- |
| `kb import <path\|url>` / 上传接口 / 监视目录 | 导入 knowledge（W2） |
| `memory consolidate [--since] [--rebuild]` | 触发整理（W4）；`--rebuild` 用于模型升级后全量重建 |
| `skills review` / `approve <name>` / `reject <name> <reason>` | 闸门（W5） |
| `memory rollback <commit>` | 用 git 回滚提炼层 |

## 演进路线

每一步都独立可用，后一步不推翻前一步的数据格式。

1. **原始层与装配**：完整 session 记录（W1）；memory/ 文件 + 模型维护的 MEMORY.md 与代码硬上限；冻结前缀。这一步先做，因为原始数据没记下的部分以后无法补。
2. **整理**：用定时器触发整理会话；进度记录；git 提交；使用统计与归档。
3. **skills**：草稿区、manual 闸门、skills 目录进入前缀。
4. **knowledge 导入**：导入命令 + 转换器 + INDEX.md；`search` 接入 FTS5（trigram 分词）。
5. **按需增强**：多作用域；向量检索 + RRF；verify 闸门；模型升级后的全量重建。

## 在 agentd 中的落地

第 1、2 步已实现，没有新增表、工具或后台循环，仍在第一阶段的规模预算内。实现时按 agentd 现有结构做了映射，与上文设计的差异如下。

| 设计 | agentd 中的实现 | 说明 |
| --- | --- | --- |
| `sessions/`（W1 记录） | `data/sessions/YYYY-MM-DD.jsonl`，按天一个文件 | 记录点在工具分发（`tool` 记录）和 worker（`run` 记录），原生循环与 OpenCode 都覆盖。写入前脱敏；`read_file` 只记参数，避免重读记录时让记录自我膨胀 |
| R3 检索 sessions | 沿用 `read_file`，路径 `sessions/YYYY-MM-DD.jsonl`，只读、可分页 | `search` 工具会成为第 7 个工具，超出预算，留待单独决策。群聊轮次一律拒绝读取，防止群成员看到所有者的私聊 |
| `memory/<topic>.md` + `MEMORY.md` | `workspace/notes/*.md` + `workspace/MEMORY.md` | 沿用已有目录和写权限，不迁移数据。索引由模型维护：写入超过 200 行或 8KB 时拒绝，装配 prompt 时按同一上限截断 |
| R1 装配、冻结前缀 | system prompt 中稳定内容在前，当前时间和最近发出的消息在末尾 | agentd 每次运行都是一次短会话，前缀在记忆不变时保持一致，可复用模型服务的前缀缓存 |
| W4 整理 | `workspace/skills/memory-consolidation/SKILL.md` + 一个定时器 | 整理是一次普通的定时运行，进度记在 `notes/memory-consolidation.md`；定时器由运维按 README 启用 |
| `session.jsonl` | 保留为本地对话的滑动缓存 | 它只负责对话连贯性，原始记录由 `data/sessions/` 承担 |

尚未实现：skill 草稿与闸门（W5）、knowledge 导入（W2）、使用统计与归档（W6）、`search` 工具、作用域分目录、提炼层的 git 版本管理。

## 参考资料

- [Anthropic: Components of agent memory](https://www.anthropic.com/research/components-of-agent-memory)：记忆层级、Dreaming、小索引、告诉模型记忆的目的
- [@reinexbt 对上文的一页总结](https://x.com/reinexbt/status/2124159642938818904)：PURPOSE / SAVE / INDEX / DREAM / WIKI / PRUNE 六步 prompt
- [Claude Code memory](https://code.claude.com/docs/en/memory)：CLAUDE.md、一事一文件 + 模型维护的 MEMORY.md 索引（加载前 200 行 / 25 KB）
- [Anthropic Agent Skills](https://platform.claude.com/docs/en/agents-and-tools/agent-skills/overview)：SKILL.md 格式与按需加载
- [Hermes Agent](https://github.com/NousResearch/hermes-agent)：有上限的 MEMORY.md / USER.md、后台复盘生成 skill、Curator 归档、FTS5 会话检索
- [Letta sleep-time agents](https://docs.letta.com/guides/agents/architectures/sleeptime)：后台整理记忆
- [Mem0](https://arxiv.org/abs/2504.19413)：ADD / UPDATE / DELETE / NOOP 对账
- [Graphiti](https://github.com/getzep/graphiti)：双时间有效期，失效而非删除
- [LangMem](https://langchain-ai.github.io/langmem/concepts/conceptual_guide/)：语义 / 情景 / 程序记忆，热路径与后台写入
- [Anthropic: Contextual Retrieval](https://www.anthropic.com/engineering/contextual-retrieval)、[sqlite-vec 混合检索](https://alexgarcia.xyz/blog/2024/sqlite-vec-hybrid-search/index.html)、[MarkItDown](https://github.com/microsoft/markitdown)、[Docling](https://docling-project.github.io/docling/)：knowledge 导入与检索
