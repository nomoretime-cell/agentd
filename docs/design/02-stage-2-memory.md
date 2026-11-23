# agentd 第二阶段：记忆模块设计（部分实现）

2026-11-23 · 在线版本（初稿，之后以本文件为准）：https://claude.ai/artifact/LJaZjYaeUwyB6RXt674s4V

> **文档状态：第二阶段设计，演进路线的第 1–4 步（含 2a 模型分级、2b 整理安全机制、skill 草稿与闸门、knowledge 导入）已实现（2026-11-23），第 5 步按需增强仍是草案。** 本文以[第一阶段架构基线](01-stage-1-architecture.md)为前提，只替换其中的 Memory 组件，其余四个组件（Inbox、Timers、Permissions、Outbox）保持不变。落地方式见文末[在 agentd 中的落地](#在-agentd-中的落地)。
>
> - **取代基线的部分**：`session.jsonl` 从"可丢弃的滑动缓存"改为"只追加的完整原始记录"（`sessions/`）；目录结构从 `workspace/notes` 改为 `memory/`、`skills/`、`skills-drafts/`、`knowledge/`；新增"整理"（由 Timers 触发的一次普通会话），不新增后台循环。
> - **与基线预算的冲突，需要单独决策**：用 SQLite FTS5 实现 `search` 会新增一张表，超出"4 张表"的预算。第一步改用文件检索（grep），可以不新增表。

## 设计目标与原则

记忆模块由四类数据和三个机制组成：原始数据（session、knowledge）只读且永久保留，提炼数据（memory、skills）是可随时重建的缓存。目标是让 agent 跨会话越用越好，同时在模型能力大幅提升时架构不失效。

七条原则，均从"模型无状态，记忆 = 未来会话的上下文里出现什么"推出：

1. **事实与派生分离。** 原始记录只追加、不修改；notes、skills、索引都是派生物。更强的模型可以从原始数据重新提炼，旧模型的错误不会永久固化（event sourcing 思路）。整理也不原地改写：每次产出一个候选版本，检查通过后才替换当前版本，旧版本保留可回滚。
2. **代码管不变量，模型管判断。** 代码负责只追加、容量上限、原子写、来源追溯、归档不删除、变更审核；记什么、怎么组织、何时沉淀为 skill 由模型决定。模型变强时只删 prompt 指导，不改代码。
3. **文件是唯一接口。** 所有记忆都是模型能用通用工具（read / write / search）直接读写的纯文本，不存在模型看不见的记忆库。
4. **加载层小而固定，按需层可增长。** 每次会话只加载有硬上限的索引与目录，详细内容按需读取。
5. **离线整理为主，在线写入为辅。** 在线写入会分散注意力且不知道任务结局；离线整理能看到完整结果，可对照原始数据校验。
6. **关键动作用最强模型。** 产出会进入之后每一次运行的动作（如整理记忆、起草 skill），由代码绑定到最强档的外部模型执行，不回退到弱模型。见[关键动作与模型分级](#关键动作与模型分级)。
7. **保留证据，不只保留结论。** 每条提炼出的事实带上用户原话和出处；被新事实推翻的旧事实标为失效、留在历史里，而不是删掉。结论会因改写而失真（例如"我这周在帮 A 公司"被改写成"用户在 A 公司工作"），原话不会。见[整理的安全机制](#整理的安全机制)。

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
| 整理（Dreaming） | 从原始数据提炼、校验、合并、归档 | session（上次进度之后）+ knowledge + 现有 memory/skills → 更新后的 memory、skill 草稿 | 一次普通的 agent 会话，只是任务 prompt 不同，且必须使用 strong 档模型；由定时器或会话结束触发 |
| 闸门 | 改变未来行为的写入需审核，可回滚 | skill 草稿、身份变更 → 生效或驳回 | 草稿区 + 审核策略 + git 版本 |

在线写入不是第四个机制：它只是模型在普通会话里用文件工具写 memory，不需要专门的记忆工具。

## 数据格式与目录结构

全部用纯文本：JSONL 存 session，Markdown + YAML frontmatter 存其余。frontmatter 只放代码需要读的字段，正文由模型自由组织。

下面是一个作用域内的**逻辑结构**。物理位置以[运行状态目录与路径约定](90-convention-storage-layout.md)为准：状态根目录是仓库下的 `.agentd/`，按角色和服务对象分目录，模型通过固定的虚拟路径访问。

```text
<一个作用域>/
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
evidence:                 # 必填：原话 + 出处，供整理校验和代码核对
  - quote: "回复简短点，术语别翻译"          # 原文照抄，不改时态和语气
    source: sessions/2026-11-20.jsonl@48213  # 文件 + 字节偏移，或 knowledge:<doc_id>
valid_until: 2027-03-31   # 可选，有时效的事实
verify: 可选，事实依赖外部状态时写明怎么复核（如"查 config.yaml 的 model.endpoint"）
updated: 2026-11-23
---
正文自由组织。可用 [[other-topic]] 链接其他条目。

## 历史
- 2026-09～2026-11-20：用户偏好详细解释。失效原因：用户改口，见 sessions/2026-11-20.jsonl@48213
```

`## 历史` 段收纳被推翻的旧事实，写明失效时间和原因；它不进入索引，但整理和检索都能看到，避免下次把旧事实当新发现重新写回来。

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
2. **提炼**：只记"看原文也看不出来"的东西：验证过的规则、计划、偏好、坑。每条附上原话和出处（evidence），证据只能来自原始层，不能来自其他 memory 条目，避免摘要的摘要层层失真。
3. **合并与失效**：相似条目合成一条；与新事实矛盾的旧条目移入该文件的 `## 历史` 段并写明失效原因，不直接删除。
4. **skill 草稿**：用户纠正过做法、同类任务重复出现、一个不简单的流程被验证有效时起草。优先修改已有 skill，最后才新建；修改前必须先读。不记环境特有的失败和偶发错误。
5. **归档与校对索引**：按使用统计归档长期未用的条目，处理过期的 valid_until，并删去 MEMORY.md 中对应的行；顺带核对 MEMORY.md 与主题文件是否一一对应。

### 整理的安全机制

整理是一次改写所有后续运行上下文的批量写入。业界的主要担心是它会"悄悄把一次的坏习惯固化成长期规则"，以及改写过程中丢掉时间和语气信息。因此整理不直接改当前版本，而是走"候选 → 检查 → 提升"：

```text
当前版本 memory/ ──复制──► 候选版本 ──整理会话改写──► 候选版本'
                                                     │
                       代码检查（确定性，必须全过）◄──┘
                         ├─ MEMORY.md 上限、frontmatter 必填字段
                         ├─ 每条新增或改动的 evidence.quote 在 source 指向的原文里逐字存在
                         ├─ 没有静默删除：消失的条目必须出现在 ## 历史 或 .archive/
                         └─ 改动量超过阈值（如一次改动超过三成条目）时转人工确认
                                                     │
                       回归探针（可选，strong 档）◄──┘
                         用候选版本回答 state/probes.jsonl 中的固定问题，
                         得分低于当前版本则不提升
                                                     │
                       提升：原子替换当前版本，保留最近 N 个旧版本，记录 diff
```

- **代码能做的检查交给代码。** 证据逐字存在、无静默删除、上限，都是字符串和文件比较，不需要模型判断，也不随模型变强而失效。
- **规则型记忆视同 skill。** "以后总是/不要……"这类会改变行为的条目，和 skill 一样进入草稿，经闸门生效；陈述性事实（偏好、环境、决策）不需要。
- **从原始层重新提炼。** 判断真假时回到 sessions 原文，不以上一次的摘要为准；模型升级后的全量重建也走同一流程。
- **使用时复核。** 带 `verify` 的事实描述的是会变的外部状态（配置、代码、账号），模型在据此执行有外部副作用的操作前先按 `verify` 复核；复核结果与记忆不符时，以复核为准并留给下次整理修正。
- **证据只来自观察到的内容。** 可引用的是运行的输入与回答、工具的返回结果；工具参数（模型自己写进文件或消息的文字）和关键动作自身的记录都不算证据，否则整理可以引用自己刚写下的内容自证。

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

## 关键动作与模型分级

**结论：先按影响面定义"关键动作"，再由代码把它们绑定到 `strong` 档模型（最强可用的外部模型）。strong 档不可用时，关键动作不执行，绝不降级到日常模型。** 宁可晚一天整理，也不让弱模型改写所有后续运行都会读到的记忆。

### 判定标准

一个动作同时满足前两条，就是关键动作：

1. **影响所有后续运行。** 产出会持久化，并进入之后每一次运行的上下文或行为：加载层（MEMORY.md、skills 目录）、skill 正文、身份规则。
2. **错误会累积且难以发现。** 后续运行会把它的产出当作事实使用，不会再对照原始数据核对。Anthropic 的实验里，一条错误的 note 让 Claude 始终过不了 Mt. Moon，直到整理时修正。
3. **不在用户等待的路径上**（决定能否负担）。关键动作多为离线、低频，可以接受强模型的延迟和成本。

### 关键动作清单

| 动作 | 对应路径 | 为什么关键 |
| --- | --- | --- |
| 记忆整理：校验、合并、替换、清理 memory 与 MEMORY.md | W4 | 改写加载层，错误会进入之后每一次运行 |
| skill 起草与修改 | W4 → skills-drafts/ | skill 会被反复照做，错误流程会被反复执行 |
| 闸门的 verify 评审 | W5 | 决定哪些草稿生效，本身就是质量把关 |
| knowledge 提炼（摘要、wiki） | W2 之后 | 提炼结果取代原文被检索和引用 |
| 模型升级后的全量重建 | 重建 | 一次性改写整个提炼层 |
| IDENTITY 变更提议 | 闸门 | 改变 agent 的基本规则 |

**不是关键动作**的：日常对话与任务执行（在用户等待的路径上，产出只影响本次）、新闻翻译等业务处理、会话中的在线记忆写入（W3）。在线写入允许由日常模型完成，但视为未确认，由下一次 strong 档整理对照原始数据校验后才算确认。这样日常模型的错误最多存活到下一次整理。

### 模型分级

- **按档位命名，不按厂商命名。** 只有两档：`default`（日常，可以是本地模型）和 `strong`（关键动作）。具体用哪个模型写在配置里，例如 OpenAI Codex 系列、Google Gemini 系列或 Anthropic Claude 系列的旗舰模型。模型换代时只改配置，不改代码。
- **绑定由代码执行。** 哪些动作是关键动作，在代码里定义；执行时按动作类型选模型，不由 prompt 或模型自己决定。
- **不回退，也不重试。** strong 档未开启或调用失败时，本次关键动作直接跳过（运行记录状态为 `skipped` 并写明原因），不改用 default 档。不重试的原因：Inbox 按顺序处理，重试耗尽后进入 dead 的事件会挡住之后所有事件；而关键动作是周期性的，下一次定时从同一进度继续，跳过一次只是晚一天。
- **写权限随档位。** skills-drafts/ 和 IDENTITY 变更提议只允许 strong 档的运行写入。

### 数据外发

关键动作要把运行记录和记忆发给外部模型服务，这些数据会离开本机。

- strong 档需要所有者在配置中显式开启，默认关闭。未开启时，关键动作不执行。
- 发送的内容沿用运行记录的脱敏；仍可能包含对话原文，README 中要写明哪些数据会发往哪个服务。
- **已决定（D1，2026-11-23）：群聊等非所有者的数据不发往外部服务。** 关键动作读取运行记录时，宿主只返回 `principal` 为 `owner`（所有者私聊）或 `local`（本机事件与定时任务）的记录；群聊、其他用户、来源不明以及作用域字段出现之前的旧记录一律留在本机，返回结果中只给出省略条数。关键动作的提示词也不包含发往其他会话的历史消息。

## 增长控制与遗忘

有害的是加载层变大，不是磁盘总量变大。加载层用代码硬封顶，提炼层靠整理保持稳定，原始层允许持续增长。

| 层 | 增长是否可接受 | 控制手段 |
| --- | --- | --- |
| 加载层（MEMORY.md、skills 目录、knowledge 索引） | 不可接受 | 代码硬上限（如索引 200 行、skills 目录 50 条）。超限时写入报错并返回当前条目，模型必须先合并。不靠 prompt 里的"请保持精简" |
| memory 正文 | 适度可接受 | 合并相似条目、矛盾即失效（移入历史段）、valid_until 到期失效、长期未读归档、能从原文查到的不记 |
| skills 正文 | 适度可接受 | 优先修改已有 skill；重叠的合并为大类；14 天未用标 stale、30 天归档（Hermes 默认值）；被定时任务引用的或人工固定的豁免 |
| sessions/、knowledge/ | 可接受 | 只检索不加载；旧数据可压缩或转冷存储 |

两条规则：

- **只归档不删除。** 归档条目移出索引但仍可检索。因为原始层永远保留，归档错了也能重新提炼，所以提炼层可以大胆删减。
- **监控撞上限的频率。** 频繁撞限说明记得太细（提高抽象层次），或作用域该拆分（见下一节）。
- **每次整理记一行指标**（写入 `state/consolidation.jsonl`）：MEMORY.md 行数与字节、本次新增 / 合并 / 失效 / 归档条数、代码检查未通过项、回归探针得分、是否提升。不看指标就无法判断记忆是在变好还是在漂移。

## 作用域、权限与闸门

### 作用域

**决策：默认隔离，跨角色只经公司共享层，经审核单向提升。** 详细目录与虚拟路径见[运行状态目录与路径约定](90-convention-storage-layout.md)。

作用域分三层，每层各有自己的 MEMORY.md 与 .archive/，各自计算上限：

| 层 | 内容 | 谁能读 | 谁能写 |
| --- | --- | --- | --- |
| 公司共享层 | knowledge（按付费等级分集合）、共享 skill、精选公司事实 | 所有角色，按等级读集合 | 仅闸门（提升）、运营人员、导入器 |
| 角色层 | 角色身份、角色 skill、角色经验记忆、原始运行记录 | 本角色 | 本角色的运行与整理 |
| 服务对象子层 | 角色内关于某个用户或群的记忆与运行记录 | 本角色内该对象的会话，以及本角色的整理 | 同左 |

跨角色值得共享的是做法（skill）和公司事实；对话中提炼的记忆和原始运行记录不跨角色。提升属于关键动作，必须去掉个人和客户信息，由 strong 档模型审核并经人确认；不能把高等级角色的内容提升到低等级可读的集合。

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
- **verify**：草稿附带 Verification，由 strong 档模型在沙箱里重跑一次来源任务，通过即生效，人事后抽查。
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

下列步骤与存储迁移、多角色等其他工作的先后顺序，见[开发路线](91-convention-roadmap.md)（第 2a、2b 步对应其中的 A3、A4）。

每一步都独立可用，后一步不推翻前一步的数据格式。

1. **原始层与装配**：完整 session 记录（W1）；memory/ 文件 + 模型维护的 MEMORY.md 与代码硬上限；冻结前缀。这一步先做，因为原始数据没记下的部分以后无法补。
2. **整理**：用定时器触发整理会话；进度记录；git 提交；使用统计与归档。
    - **2a. 模型分级**：配置 strong 档，整理等关键动作绑定 strong 档且不回退。
    - **2b. 整理的安全机制**：候选版本、代码检查（证据逐字存在、无静默删除、上限、改动量阈值）、提升与旧版本保留、整理指标；memory 文件加 `evidence` 和 `## 历史`。整理定时器应在 2a、2b 都完成后再启用。
3. **skills**：草稿区、manual 闸门、skills 目录进入前缀；规则型记忆走同一闸门。
4. **knowledge 导入**：导入命令 + 转换器 + INDEX.md；`search` 接入 FTS5（trigram 分词）。
5. **按需增强**：多作用域；向量检索 + RRF（混合检索在公开评测中是收益最大的单项改进）；回归探针；verify 闸门；模型升级后的全量重建。

## 在 agentd 中的落地

第 1、2 步已实现，没有新增表、工具或后台循环，仍在第一阶段的规模预算内。实现时按 agentd 现有结构做了映射，与上文设计的差异如下。下表是**当前**的物理路径；迁移到 `.agentd/` 和按角色分目录后的位置，见[运行状态目录与路径约定](90-convention-storage-layout.md)第 6 节。

| 设计 | agentd 中的实现 | 说明 |
| --- | --- | --- |
| `sessions/`（W1 记录） | `data/sessions/YYYY-MM-DD.jsonl`，按天一个文件；每条记录带 `role`、`principal`、`conversation` 作用域字段 | 记录点在工具分发（`tool` 记录）和 worker（`run` 记录），原生循环与 OpenCode 都覆盖。OpenCode 自带的工具（如 websearch）按其事件记录名称、状态、输入，以及截断到 2KB 的输出或失败原因，事件里没有的内容记录不到。写入前脱敏；`read_file` 只记参数，读取运行记录本身不记录，避免记录自我膨胀 |
| R3 检索 | `search` 工具（第 7 个基础工具，D2）：对 `sessions/` 与 `notes/` 逐行子串扫描，同一行包含全部关键词即命中，不建索引、不加表；返回路径、偏移、片段，再用 `read_file` 按偏移细读。`read_file` 读运行记录按偏移分页，不受工作区文件 1 MiB 上限约束 | 作用域与读取相同：回复会被其他群成员看到的轮次不能检索或读取运行记录；关键动作只检索到 owner / local 记录。检索调用本身不写入运行记录，否则查询词会命中自己。数据量大到扫描变慢时再换 FTS5 |
| `memory/<topic>.md` + `MEMORY.md` | `workspace/notes/*.md` + `workspace/MEMORY.md` | 沿用已有目录和写权限，不迁移数据。索引由模型维护：写入超过 200 行或 8KB 时拒绝，装配 prompt 时按同一上限截断 |
| R1 装配、冻结前缀 | system prompt 中稳定内容在前，当前时间和最近发出的消息在末尾 | agentd 每次运行都是一次短会话，前缀在记忆不变时保持一致，可复用模型服务的前缀缓存 |
| 关键动作与模型分级（2a） | 配置 `strong_model`（默认关闭）；保留定时器 ID `memory-consolidation` 产生 `consolidation` 事件，只由 strong 档执行，只提供 `read_file`、`write_file`、`edit_file`；运行记录按 D1 过滤；失败记为 `skipped` | 关键动作的种类、可用工具和数据范围都在代码中定义（`loop.go`），不由提示词决定。strong 档只发送标准 chat-completions 字段，兼容 OpenAI、Gemini 等托管服务的接口；步数、时长、上下文预算单独配置 |
| W4 整理 | 内置 skill `skills/memory-consolidation/SKILL.md`（`internal/agent/defaults/skills/`）+ 一个定时器 | 整理是一次普通的定时运行，进度记在 `notes/memory-consolidation.md`。目前仍使用默认模型，不符合"关键动作用最强模型"，因此定时器默认不启用，待 2a 完成后再启用 |
| `session.jsonl` | 保留为本地对话的滑动缓存 | 它只负责对话连贯性，原始记录由 `data/sessions/` 承担 |

尚未实现：使用统计与归档（W6）、作用域分目录、FTS5 与向量检索、回归探针、verify / auto 闸门、模型升级后的全量重建。

第 4 步 knowledge 的实现（2026-11-23，`knowledge.go`）：

- **导入**：`POST /v1/knowledge/import {"path": ...}` 在后台导入一个文件或整个目录（跳过隐藏目录与不支持的格式），同一时间只有一个导入。md、markdown、txt 直接导入；pdf、docx、pptx、xlsx、html、csv、epub 交给配置的转换器（如 MarkItDown），转换器在与 `bash` 相同的沙箱中运行：无网络，只能读系统目录、转换器自身目录和文件副本。没配置转换器时这些格式会被拒绝并说明原因。
- **存放**：转换后的 Markdown 在 `workspace/knowledge/md/<doc_id>.md`，模型只读；原文件在 `data/knowledge/raw/`，模型不可见；清单 `data/knowledge/manifest.json` 记录来源、sha256、大小、转换器、导入时间；每次导入的结果写入 `data/knowledge/imports.jsonl`。`doc_id` 由来源路径确定。
- **去重与更新**：hash 未变则跳过；变了则替换 Markdown，旧的移入 `knowledge/.archive/`；导入目录时，之前从该目录导入、但文件已不存在的文档归档并移出索引。`POST /v1/knowledge/{doc_id}/remove` 手动归档。
- **索引**：代码按清单生成 `knowledge/INDEX.md`（一行一个文档：标题、路径、大小），与 MEMORY.md 同一上限（200 行 / 8KB），超限的那个文件导入失败；索引装进每次运行的 system prompt。
- **检索与证据**：`search` 覆盖 `knowledge/`（不含归档）；整理可以引用 `knowledge/md/<doc_id>.md` 中的原文作为 evidence。
- **限制**：单个文件不超过 50MB，转换后不超过 1MiB（`read_file` 的上限），超过需拆分；不支持 URL 导入；集合与付费等级在第三阶段引入。

第 3 步 skills 的实现（2026-11-23，`gate.go`）：

- **草稿只由关键动作写**：`skills-drafts/` 只有关键动作（整理）能读写，普通运行读、写、检索都被拒绝，所以草稿批准前不会进入任何运行的上下文。整理写的草稿和记忆一样先进候选副本，候选被拒绝时草稿一并作废，候选待确认时草稿一并等待。
- **闸门为 manual**：`GET /v1/drafts` 列出草稿和批准时会遇到的问题；`POST /v1/drafts/{name}/approve` 安装到 `workspace/skills/<name>/`，被替换的旧 skill 存入 `data/gate/versions/`；`/reject` 必须写理由，草稿连同 `REJECTED.md` 移到 `skills-drafts/.rejected/`，下次整理起草前先读。每个决定一行写入 `data/gate/decisions.jsonl`。批准时校验：名称与目录一致且为小写加连字符、有 description、总大小不超过 256KB、不含符号链接、skill 总数不超过 50。
- **skills 目录进入前缀**：每次运行的 system prompt 列出全部可用 skill 的 name 与 description（内置的和 `workspace/skills/` 的，后者同名优先），最多 50 条、4KB，按名称排序以保住前缀缓存。
- **规则型记忆走同一闸门**：整理把"以后总是/不要"类规则写成 `skills-drafts/identity/PROPOSAL.md`；批准后追加到 IDENTITY.md 的"Approved rules"一节，旧的 IDENTITY.md 存入 `data/gate/versions/`。
- **与设计的差异**：verify / auto 闸门未实现；草稿不要求 evidence，由人审核；已批准 skill 的使用统计与 stale 归档（W6）未实现。

2b 的实现（2026-11-23，`consolidation.go`）与上文设计的差异：

- **版本用目录而不是 git。** 候选在 `data/memory/candidate/`，提升时当前版本移入 `data/memory/versions/<时间戳>/`（保留 10 个），提升过程有标记文件，崩溃后下次启动补完。回滚把上一版本提升回来，当前版本同样留作版本，所以回滚本身可撤销。
- **检查项**：MEMORY.md 上限；不得删除笔记文件；归档不可改；改动或新增的笔记必须有 evidence，每条引文至少 4 个字符，且逐字出现在所引运行记录中、本次运行可见（owner / local）的输入、回答或工具结果里；`## 历史` 只增不减。改动超过现有条目 30%（至少 5 条时）或删去 MEMORY.md 30% 以上的行（至少 10 行时）转为待确认。
- **结果**：通过则提升（`promoted`）；没有改动则不产生版本（`unchanged`）；检查失败则丢弃并留一份在 `data/memory/rejected/`（`rejected`，运行记为 `skipped`）；需确认则移到 `data/memory/held/` 并通知默认目标（`held`），由 `POST /v1/memory/promote` 或 `/discard` 处理。候选记录生成时的记忆摘要（`base.sha256`），提升前核对：等待期间记忆有改动则候选已过期，提升返回 409，避免覆盖普通运行新写的笔记。待确认的候选未过期时，后续整理跳过；已过期时，下一次整理开始时自动丢弃它（`discarded`）再照常整理，所以不处理通知也不会让整理一直停住。以后若过期丢弃频繁，可改为按文件三方合并。每次一行指标写入 `data/memory/consolidation.jsonl`。
- **规则型记忆**：skill 闸门（B1）之前，整理把这类条目写入 `notes/rule-proposals.md` 供所有者查看，不写进索引。这一条靠 skill 约束，代码不检查，B1 时改为走闸门。
- **未实现**：回归探针、`verify` 字段的使用时复核、knowledge 作为证据来源（`knowledge:<doc_id>` 目前会被拒绝）。
- 达到步数或上下文上限而停止的运行仍会检查并提升已完成的部分；模型调用失败等其他错误则整体丢弃。

## 与业界做法的对照（2026-11）

| 方面 | 业界做法 | 本设计 |
| --- | --- | --- |
| 加载层 | Claude Code：一事一文件 + 模型维护的 MEMORY.md，按行数和字节截断 | 相同，且写入时拒绝超限 |
| 后台整理 | Claude Managed Agents dreaming：输入记忆库 + 1～100 个会话，输出一个**新的**记忆库，原库不变，人审后再切换；Letta sleep-time | 候选版本 + 提升，和 dreaming 一致；另加代码检查和回归探针，dreaming 文档未提供这类检查 |
| 原始数据 | 多数产品只保留提炼后的记忆，会话按保留期过期 | 原始层永久保留，提炼层可重建，这一点比多数产品更严格 |
| 矛盾与时间 | Graphiti / Zep 用有效期字段标失效而非删除；LAPSE 研究发现主流系统改写时会丢失时态，建议保留原话 | 历史段 + evidence 原话 |
| 使用前复核 | GitHub Copilot memory 带代码引用，行动前对照当前分支复核 | `verify` 字段，仅用于会变的外部状态 |
| 程序性记忆 | Hermes：后台复盘生成 skill，Curator 归档；Agent Skills 格式 | 相同，生效走闸门 |
| 检索 | 混合检索（稀疏 + 稠密）在控制检索预算后效果最好；各家排行榜差距多来自检索预算 | 先 FTS5，再按需加向量 + RRF；检索结果有长度上限 |
| 尚未覆盖 | 实体解析、知识图谱、学习到的重要性评分 | 有意不做：依赖特定模型或 schema，模型变强后容易需要重写 |

## 参考资料

- [Anthropic: Components of agent memory](https://www.anthropic.com/research/components-of-agent-memory)：记忆层级、Dreaming、小索引、告诉模型记忆的目的
- [@reinexbt 对上文的一页总结](https://x.com/reinexbt/status/2124159642938818904)：PURPOSE / SAVE / INDEX / DREAM / WIKI / PRUNE 六步 prompt
- [Claude Code memory](https://code.claude.com/docs/en/memory)：CLAUDE.md、一事一文件 + 模型维护的 MEMORY.md 索引（加载前 200 行 / 25 KB）
- [Anthropic Agent Skills](https://platform.claude.com/docs/en/agents-and-tools/agent-skills/overview)：SKILL.md 格式与按需加载
- [Hermes Agent](https://github.com/NousResearch/hermes-agent)：有上限的 MEMORY.md / USER.md、后台复盘生成 skill、Curator 归档、FTS5 会话检索
- [Claude Managed Agents: dreams](https://platform.claude.com/docs/en/managed-agents/dreams)：整理输出新记忆库、不修改输入
- [LAPSE: Memory Consolidation Flattens the Temporal Shape of User Facts](https://arxiv.org/abs/2609.36457)：改写丢失时态，建议保留原话作证据
- [Hindsight: The Consolidation Problem in Agent Memory](https://hindsight.vectorize.io/blog/2026/05/21/agent-memory-consolidation)：recency-wins + 显式失效、衰减与淘汰的取舍
- [Beyond Memory Leaderboards](https://arxiv.org/abs/2607.16848)：控制检索预算后的对比，混合检索收益最大
- [Episodic-to-semantic consolidation 综述](https://davidamitchell.github.io/Research/research/2026-07-20-agent-memory-consolidation-episodic-semantic.html)：Copilot memory 的引用与行动前复核
- [Letta sleep-time agents](https://docs.letta.com/guides/agents/architectures/sleeptime)：后台整理记忆
- [Mem0](https://arxiv.org/abs/2504.19413)：ADD / UPDATE / DELETE / NOOP 对账
- [Graphiti](https://github.com/getzep/graphiti)：双时间有效期，失效而非删除
- [LangMem](https://langchain-ai.github.io/langmem/concepts/conceptual_guide/)：语义 / 情景 / 程序记忆，热路径与后台写入
- [Anthropic: Contextual Retrieval](https://www.anthropic.com/engineering/contextual-retrieval)、[sqlite-vec 混合检索](https://alexgarcia.xyz/blog/2024/sqlite-vec-hybrid-search/index.html)、[MarkItDown](https://github.com/microsoft/markitdown)、[Docling](https://docling-project.github.io/docling/)：knowledge 导入与检索
