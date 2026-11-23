# Inkling 2 Mini 与 Qwen3.8-27B DFlash 2 本机对比

测试日期为 2026-11-22。设备为 Apple M5 Max、40 核 GPU、64 GiB 统一内存，系统为 macOS 27.2.1。测试对象是 agentd 实际可调用的两套本地 HTTP 推理服务。

## 结论与实测

**以这台 Mac 的中文聊天速度为首要目标，建议 agentd 继续使用当前 Qwen3.8-27B + DFlash 2。** 在同样的短输入下，Qwen 完成 128 tokens 用时 1.67 秒，Inkling 用时 3.27 秒。输入一批工单时，Inkling 首个正文字符的中位数早 0.59 秒，但写完 256 tokens 仍晚 2.20 秒。

| 场景（每项 5 次，取中位数） | 服务 | 正文首字 | 生成速度 | 总耗时 | 每秒正文字符¹ |
| --- | --- | ---: | ---: | ---: | ---: |
| 短提示，实出 128 tokens | Qwen + Splash DFlash 2 | 0.077 秒 | 79.7 tok/s | 1.67 秒 | 151 |
| 同一短提示，实出 128 tokens | Inkling MLX 4-bit，直接回答 | 0.249 秒 | 42.0 tok/s | 3.27 秒 | 49 |
| 170 条工单，实出 256 tokens | Qwen + Splash DFlash 2 | 1.637 秒 | 74.9 tok/s | 5.04 秒 | 84 |
| 同一批工单，实出 256 tokens | Inkling MLX 4-bit，直接回答 | 1.050 秒 | 41.0 tok/s | 7.25 秒 | 44 |

¹ 每秒正文字符按返回文本字符数除以端到端耗时计算，含标点和空白。两者有各自的中文分词器，也可能写出不同内容。短提示对应 Qwen 50、Inkling 58 个输入 tokens；长提示对应 Qwen 2,599、Inkling 2,052–2,061 个输入 tokens。上表各次 API 报告的缓存命中均为 0。短提示补测统一用 128 tokens，因为在最初的 256 上限下，Inkling 有 4/5 次提前结束。各行实际输出长度一致后再对比耗时。

**基本对话。** 直接回答模式下，两端都给出了杭州西湖散步建议，并在追问前文城市时答出“杭州”。另有一个可复现的问题：Inkling 在最初 5 次短提示测试中，有 4 次以“不能直接生成这份指南”开头，并只给框架；加上工单资料的 5 次长提示都开始撰写指南。日常问答能用，指令执行仍有明显波动。此次没有测通用准确率。

**模板会改变用户看到的速度。** 在社区原始 MLX 模板下，即使指定 `reasoning_effort=none`，接口仍将思考片段和协议标记放进 `content`。短提示的首个原始数据块中位数为 0.258 秒，但等到真正的正文需 5.39 秒；5 次长提示均在 256 tokens 用尽前未进入正文。上表的“直接回答”使用同一份权重，只在无工具、无思考的文本请求末尾预填原生助手正文头 `<|message_model|><|content_text|>`，客户端用 `<|end_message|>` 停止生成。这个调整没有更换或修改权重，也没有验证工具模式。

## 模型与运行时

| | Inkling 2 Mini | Qwen3.8-27B |
| --- | --- | --- |
| 权重 | mlx-community/Inkling-2-Mini-4bit，固定版本 `9856dc53edead71405f8348930937c3638ff21a2` | 现有 `Qwen3.8-27B-4bit` 及 `Qwen3.8-27B-DFlash2` 草稿 |
| 架构与推理 | 26.4B MoE，每 token 激活 3.4B 参数，核心层循环使实际权重激活量为 5.4B。此次使用标准自回归解码 | 27B 主模型，以 DFlash 2 草稿做推测解码 |
| 环境 | 复用 `/Users/yejibing/code/mlx-lm/.venv`，MLX 0.32.4，上游 MLX-LM PR #2086 固定在 `0c4c99192230f1943dd468d297f0b8a64ae7b7c2` | 已在 8080 常驻的 Splash 1.7.0，64K 上下文，BF16 KV |
| 本地位置 | `/Users/yejibing/models/Inkling-2-Mini-4bit` | `/Users/yejibing/models/Qwen3.8-27B-4bit` |
| 使用场景 | 独立工作树运行在本地回环端口 18081，与 agentd 线上端口隔离 | 原服务、模型和端口沿用现场配置 |

**选型理由。** MLX 社区的 4-bit 版保留路由专家原有的精度分配，前 20 层使用 NVFP4，末 4 层使用 MXFP8。注意力、嵌入和输出头采用分组大小 64 的 4-bit affine 量化，视觉塔保留 BF16，路由器保留 FP32。四个权重分片共 17,308,326,283 字节，约 16.12 GiB。它与 Apple Silicon 上已有的 Python 和 Metal 环境兼容，且社区给出了具体测试分支。这是结合本机与流量预算的选择，只评估这一个 Inkling 量化；没有证据给所有量化版本做排名。[MLX 社区模型卡](https://huggingface.co/mlx-community/Inkling-2-Mini-4bit) · [运行时提交](https://github.com/ml-explore/mlx-lm/commit/0c4c99192230f1943dd468d297f0b8a64ae7b7c2) · [Hugging Face 推理说明](https://huggingface.co/blog/inkling2mini)

**本机精度设置。** 上游模型单元测试在这台 M5 Max 的默认 MLX 环境有 3/8 项失败（批处理、KV 缓存、循环层与参考计算的一致性），内核测试有 1/11 项失败。仅给 Inkling 进程设置 `MLX_ENABLE_TF32=0` 后，模型 8/8 和内核 11/11 全部通过。因此正式测速使用该设置；数值自测不能替代真实任务评分。[上游测试代码](https://github.com/ml-explore/mlx-lm/blob/0c4c99192230f1943dd468d297f0b8a64ae7b7c2/tests/models/test_inkling2.py)

## 测量方法

- 使用同一台 Mac，客户端走本机 HTTP 流式 API，串行请求，先预热，温度设为 0。Qwen 使用当前服务，Inkling 另起进程。测得的差异同时包含模型、分词器、量化和运行时的影响。Qwen 的收益包含 DFlash；所选 Inkling 权重包不含 DSpark 草稿。
- **交互速度**使用短提示和带 170 条中文工单的提示，每档 5 个输入，分别报告中位数。每次要求生成长篇中文操作指南，短提示的等长复测使用 128 tokens，长提示使用 256 tokens。测量从发送请求起算。近似生成速率为（输出 token 数减 1）除以（总耗时减首个非空内容块等待时间）。分词和流式分块会影响指标，比较用户体验时应同时看总耗时。
- **对话检查**给两端同一条中文提问，让模型用两句话推荐杭州西湖散步的出发时间，再附上真实的上轮回答，追问之前提到的城市。人工检查可读性、建议是否切题及能否接上“杭州”。它是基础可用性检查，没有泛化为准确率。
- 两端均测直接回答模式。Qwen 使用 agentd 当前的 `enable_thinking=false`，Inkling 使用原生模板的 `reasoning_effort=none`。速度测试只覆盖文本生成和单并发。未比较视觉、多用户吞吐、完整 OpenCode 工具链和 32K 以上长上下文。
- 调整测试范围前已完成的 Qwen 单侧题集与中等思考结果保存在原始目录，仅供复查。表中的短提示取自 `qwen-short128.jsonl` 和 `mlx4-direct-short128.jsonl`，长提示取自 `qwen-perf.jsonl` 和 `mlx4-direct-perf.jsonl`。社区默认模板的对照保存在 `mlx4-perf.jsonl`。

## 发布方评测的参照

Thinking Machines 报告的 DeepSWE v1.1 为 Inkling 48.2%、Qwen 42.2%，Terminal-Bench v2.1 为 71.6%、73.0%。这两套任务上的领先者不同。发布方的运行框架、思考预算与此次本地量化服务各异，因此这些数字单列作背景。[发布方能力与评测设置](https://thinkingmachines.ai/news/inkling-2-mini/)

## 在本机复现

已有 Qwen API 位于 `http://127.0.0.1:8080/v1`，测试完成后仍是 agentd 的默认后端。Inkling 测试服务已释放。要临时启动经过验证的直接回答配置，可运行下列命令。它先离线校验四个分片，再使用前述 MLX 环境监听 `127.0.0.1:18081`。

```sh
cd /Users/yejibing/models/benchmarks/20261122-inkling
python3 verify-and-run.py --direct --serve
```

在另一终端试用，结束时在服务终端按 Ctrl+C。

```sh
curl -sS http://127.0.0.1:18081/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"/Users/yejibing/models/Inkling-2-Mini-4bit","messages":[{"role":"user","content":"你好，杭州西湖适合什么时候去？"}],"temperature":0,"max_tokens":256,"stop":["<|end_message|>"]}'
```

[复测脚本](/Users/yejibing/models/benchmarks/20261122-inkling/verify-and-run.py) 在不带 `--serve` 时自动启动和关闭测试服务；`--direct` 跑直接回答模式，增加 `--short128` 复现等长短提示。Qwen 使用同目录的 `python3 bench.py qwen 8080 /Users/yejibing/models/Qwen3.8-27B-4bit --perf`，同样可追加 `--short128`。对照中 Qwen 一直驻留；初次启动 Inkling 后系统交换空间用量由约 1.9 GiB 上升到约 5.5 GiB。没有单独卸载 Qwen 来测 Inkling 的独占性能。原始请求、回复与计时可逐项核查。[原始数据目录](/Users/yejibing/models/benchmarks/20261122-inkling) · [权重校验清单](/Users/yejibing/models/benchmarks/20261122-inkling/verified.json)
