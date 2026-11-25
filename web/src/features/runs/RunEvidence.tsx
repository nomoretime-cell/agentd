import { z } from "zod"

export const evidenceSchema = z.object({
  sources: z
    .array(
      z.object({
        kind: z.string(),
        name: z.string(),
        version: z.string(),
        digest: z.string(),
        text: z.string(),
        truncated: z.boolean(),
        offset: z.number(),
        end: z.number(),
      })
    )
    .nullable(),
  uses: z.array(z.object({ step: z.string(), source: z.number() })).nullable(),
  omitted: z.number(),
})
const groups = [
  { label: "随请求提供", kinds: ["document", "catalog", "history", "request"] },
  {
    label: "工具调用与实际返回",
    kinds: ["tool_args", "tool", "tool_error", "executor", "read", "hit"],
  },
  { label: "业务采用的来源", kinds: ["business"] },
]
const labels: Record<string, string> = {
  document: "文档",
  catalog: "技能目录",
  history: "历史消息",
  request: "请求消息",
  tool_args: "调用参数",
  tool: "返回值",
  tool_error: "工具错误",
  executor: "执行器报告",
  read: "文件读取",
  hit: "检索命中",
  business: "报道",
}
function mentionsPath(answer: string, path: string) {
  if (!path) return false
  const escaped = path.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")
  return new RegExp(
    "(?:^|[^A-Za-z0-9_./-])" + escaped + "(?=$|[^A-Za-z0-9_./-]|\\.(?:\\s|$))"
  ).test(answer)
}
export function RunEvidence({
  data,
  answer,
}: {
  data: z.infer<typeof evidenceSchema> | null
  answer: string
}) {
  const sources = data?.sources ?? []
  const mentioned = sources.filter(
    (s) =>
      ["read", "hit", "document"].includes(s.kind) &&
      mentionsPath(answer, s.name)
  )
  return (
    <section className="evidence">
      <h3>上下文依据</h3>
      {!sources.length && (
        <p>
          {data
            ? "本次没有采集到可展示的输入或工具回执。"
            : "这次尝试没有留存上下文快照。"}
        </p>
      )}
      {groups.map((g) => {
        const rows = sources
          .map((s, index) => ({ s, index }))
          .filter(({ s }) => g.kinds.includes(s.kind))
        return (
          rows.length > 0 && (
            <div key={g.label}>
              <h4>{g.label}</h4>
              {rows.map(({ s, index }) => (
                <details key={index}>
                  <summary>
                    {labels[s.kind] ?? s.kind} · {s.name}
                  </summary>
                  <small>
                    步骤{" "}
                    {Array.from(
                      new Set(
                        data?.uses
                          ?.filter((u) => u.source === index)
                          .map((u) => u.step)
                      )
                    ).join("、") || "未记录"}
                  </small>
                  {s.version && (
                    <small>
                      来源版本 SHA-256 <code>{s.version}</code>
                    </small>
                  )}
                  <small>
                    脱敏内容 SHA-256 <code>{s.digest}</code>
                  </small>
                  {s.kind === "read" && (
                    <small>
                      读取字节 [{s.offset}, {s.end})
                    </small>
                  )}
                  {s.kind === "hit" && <small>命中字节偏移 {s.offset}</small>}
                  <pre>{s.text || "空内容"}</pre>
                  {s.truncated && <small>摘录已截断</small>}
                  {["read", "document", "hit"].includes(s.kind) &&
                    /^(MEMORY\.md$|IDENTITY\.md$|notes\/|skills\/)/.test(
                      s.name
                    ) && (
                      <a
                        href={
                          "#evolution?" + new URLSearchParams({ path: s.name })
                        }
                      >
                        查看当前条目与修订记录
                      </a>
                    )}
                </details>
              ))}
            </div>
          )
        )
      })}
      {sources.length > 0 && (
        <p>
          回答中明确出现的来源路径 ·{" "}
          {Array.from(new Set(mentioned.map((s) => s.name))).join("、") ||
            "未发现"}
        </p>
      )}
      {!!data?.omitted && <p>超出留存上限的引用 · {data.omitted} 项</p>}
    </section>
  )
}
