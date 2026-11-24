# agentd 运行状态目录与路径约定

2026-11-23 · 适用于所有阶段。代码、设计文档和脚本中的运行时路径，都以本文为准。

> **文档状态：目标布局已决定；迁移第 1、2 步已实施（2026-11-23）：状态根目录默认是 `.agentd/`，运行记录带作用域字段。** 角色分目录（第 3 步）尚未实施，`.agentd/` 内部仍是单角色的 `data/`、`workspace/`。本文第 6 节列出当前位置与目标位置的对照，第 7 节列出迁移步骤。新代码按"相对状态根目录"的约定编写（第 5 节）。

## 1. 结论

- **运行状态只有一个根目录：仓库下的 `.agentd/`**，由 `-root` 指定。代码仓库只放代码和随版本发布的默认内容。
- **生产部署**可以把整个目录搬到 `/var/lib/agentd`，配置放 `/etc/agentd/config.yaml`，启动参数 `-root /var/lib/agentd`。根目录内部结构不变。
- **目录就是权限边界。** 一次运行能访问哪些目录，在运行开始时由宿主根据"当前角色 + 服务对象"决定；模型只看到一套固定的虚拟路径（第 3 节），写不出其他角色的路径。

## 2. 目标布局

```text
<repo>/                                  # 代码仓库
├── cmd/  internal/  docs/  scripts/     # 代码与随版本发布的默认内容
├── .tools/  .cache/                     # 工具链与构建缓存（已有）
└── .agentd/                             # 状态根目录（-root），整个目录被 git 忽略，整体备份
    ├── data/                            # 宿主私有：模型不能直接访问
    │   ├── state.db                     # 4 张业务表；运行快照、回复、调用和投递回执归属 Inbox / Outbox
    │   ├── workbench-since               # 工作台指标开始采集的 UTC 时间，随数据库备份
    │   ├── agentd.lock  paused.lock  api-token
    │   ├── deliveries/  outputs/  opencode/
    │   ├── memory/                      # 整理的候选、待确认、被拒版本与历史版本，模型不可见
    │   │   ├── candidate/  held/  rejected/  versions/<时间戳>/
    │   │   │   # candidate / held 内的 base.sha256 记录生成候选时的记忆快照摘要
    │   │   └── promoting.json  consolidation.jsonl   # 提升进行中的标记；每次整理一行指标
    │   ├── gate/                        # 闸门：decisions.jsonl 决定记录；versions/ 被替换的 skill 与 IDENTITY.md
    │   ├── knowledge/                   # 导入：raw/ 原文件、manifest.json 清单、imports.jsonl 导入记录、tmp/ 转换工作区
    │   ├── principals.jsonl             # 服务对象标识与平台 ID 的对应，供人查阅
    │   └── roles/<role>/sessions/<principal>/YYYY-MM-DD.jsonl   # 原始运行记录
    └── workspace/                       # 模型可见内容的物理位置
        ├── company/                     # 公司共享层：所有角色只读，只经闸门写入
        │   ├── knowledge/<collection>/  # 每个集合对应一个付费等级：INDEX.md raw/ md/
        │   ├── skills/<name>/SKILL.md   # 共享 skill
        │   └── MEMORY.md  notes/        # 精选公司事实，受索引上限约束
        └── roles/<role>/                # 角色层：只有本角色能读写
            ├── IDENTITY.md              # 角色身份规则（人维护）
            ├── skills/  skills-drafts/  # 角色 skill；等待闸门的草稿
            ├── MEMORY.md  notes/        # 角色经验记忆
            ├── principals/<principal>/  # 服务对象子层：MEMORY.md notes/
            └── scratch/
```

| 目录 | 写入者 | 模型可见 | 说明 |
| --- | --- | --- | --- |
| `data/` | 宿主 | 否，仅经宿主函数按权限读取 | 事实与凭证；`sessions` 只追加 |
| `workspace/company/` | 闸门、运营人员、导入器 | 只读 | 跨角色共享的唯一通道 |
| `workspace/roles/<role>/` | 本角色的运行、整理、运营人员 | 仅本角色 | `IDENTITY.md` 与 `skills/` 对模型只读 |

**标识命名：**

- `<role>`：在配置中声明，只允许 `[a-z0-9-]`。只有一个角色时固定为 `default`。
- `<principal>`：由宿主根据身份生成，取值为 `owner`、`local`、`user-<hash>`、`group-<hash>`，其中 `<hash>` 用 `stableID`。平台 ID 不进路径，避免特殊字符和账号暴露；对应关系记在 `data/principals.jsonl`。

## 3. 模型看到的虚拟路径

模型使用的路径与当前实现一致，prompt 和 skill 不需要随目录调整而修改。宿主在每次运行开始时按作用域映射：

| 模型路径 | 物理位置 | 权限 |
| --- | --- | --- |
| `IDENTITY.md` | `workspace/roles/<role>/IDENTITY.md` | 只读 |
| `MEMORY.md`、`notes/` | `workspace/roles/<role>/` | 读写 |
| `principal/MEMORY.md`、`principal/notes/` | `workspace/roles/<role>/principals/<principal>/` | 读写；仅服务对象明确时存在 |
| `skills/` | 角色 `skills/` 与 `company/skills/` 的合并只读视图 | 只读 |
| `company/...` | `workspace/company/` 中本角色等级允许的部分 | 只读 |
| `sessions/YYYY-MM-DD.jsonl` | `data/roles/<role>/sessions/<principal>/` | 只读 |
| `scratch/`、`outputs/` | `workspace/roles/<role>/scratch/`、`data/outputs/` | 读写 / 只读 |

整理运行的作用域是整个角色：可以读本角色所有服务对象的运行记录，但读不到其他角色的任何内容。群聊轮次的作用域是这个群，只能读这个群自己的运行记录。

## 4. 作用域与跨角色规则

- **默认隔离。** 运行只加载当前角色、当前服务对象和公司共享层。
- **共享只经公司共享层。** 角色之间不直接读写彼此的目录。
- **单向提升。** 角色整理时发现通用经验，起草提升申请；去掉个人和客户信息、经 strong 档模型审核并由人确认后，写入 `company/`。提升属于关键动作，见[第二阶段设计](02-stage-2-memory.md#关键动作与模型分级)。
- **付费等级。** 角色绑定等级，等级决定能读 `company/knowledge/` 中的哪些集合。提升不能把高等级角色的内容写入低等级可读的集合。
- **原始运行记录绝不跨角色读取。**

## 5. 开发约定

1. **运行时路径一律相对状态根目录拼接**（代码中的 `s.root`），不写死仓库路径、`.agentd` 或 `/var/lib/agentd`。
2. **模型可见路径只经宿主函数映射**：`workspaceRead`、`workspaceWrite`、`transcriptRead`，以及迁移后的作用域映射函数。工具代码不自行拼接物理路径。
3. **`data/` 下的内容不经工具直接暴露**，只能通过上述函数按权限读取。
4. **随版本发布的默认内容**放在 `internal/agent/defaults/`，用 `go:embed` 编入程序。模板（`IDENTITY.md`、`MEMORY.md`）首次启动时目标文件不存在才复制，之后归运营人员和模型，不覆盖。内置 skill 不复制，由宿主以只读方式直接从程序提供，这样升级程序就升级 skill；`workspace/skills/` 下的同名文件优先，供运营人员覆盖。
5. **程序和工具的相对路径**（如 `opencode_binary`）相对启动时的工作目录解析，不相对状态根目录。
6. **新增任何运行时路径，先更新本文**，再写代码。

## 6. 当前位置与目标位置对照

| 当前位置（`-root .`） | 目标位置（`.agentd/` 下） | 何时迁移 |
| --- | --- | --- |
| `data/state.db` 等宿主文件 | `data/` 同名文件 | 第 1 步 ✅ |
| `data/sessions/YYYY-MM-DD.jsonl` | `data/roles/default/sessions/_legacy/` 原样保留；旧记录没有服务对象字段，只对整理运行可见 | 第 3 步 |
| `workspace/IDENTITY.md` | `workspace/roles/default/IDENTITY.md` | 第 3 步 |
| `workspace/MEMORY.md`、`workspace/notes/` | `workspace/roles/default/` | 第 3 步 |
| `workspace/knowledge/`（`INDEX.md`、`md/`、`.archive/`）、`data/knowledge/` | `workspace/company/knowledge/<collection>/`、`data/knowledge/`；集合与付费等级在第 4 步引入 | 第 3、4 步 |
| `workspace/skills-drafts/`（含 `.rejected/`）、`data/gate/` | `workspace/roles/default/skills-drafts/`、`data/roles/default/gate/` | 第 3 步 |
| `workspace/skills/`（仓库中的默认 skill） | 已编入程序（`internal/agent/defaults/skills/`），以 `skills/` 只读提供；第 3 步起作为 `company/skills/` 的内置部分 | 第 1 步 ✅ |
| `workspace/scratch/` | `workspace/roles/default/scratch/` | 第 3 步 |

`data/memory/` 在第 3 步随角色分目录移到 `data/roles/<role>/memory/`。

## 7. 迁移步骤

1. ✅ **引入 `.agentd/`**（2026-11-23）：`-root` 默认值改为 `.agentd`；默认内容改为 `go:embed`；`opencode_binary` 等改为相对工作目录；README、脚本中的 `data/api-token` 改为 `.agentd/data/api-token`；`.gitignore` 加入 `.agentd/`。
   - **已有实例**：启动时如果检测到旧布局（当前目录下有 `data/state.db`，但没有 `.agentd/`），就停止启动，并提示两种做法：执行 `mkdir .agentd && mv data workspace .agentd/` 完成迁移，或者显式传 `-root .` 继续使用旧布局。不自动移动数据。
2. ✅ **运行记录带作用域字段**（2026-11-23）：每条记录增加 `role`（固定为 `default`）、`principal`、`conversation`。不改目录，不改已有字段。
3. **第二个角色出现时**：建立 `company/`、`roles/` 和虚拟路径映射，把单角色时的内容迁入 `roles/default/`。
4. **第三阶段**：付费等级与知识集合的权限、提升闸门。
