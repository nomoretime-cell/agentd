import { z } from "zod"
export const delta = z.object({
  path: z.string(),
  before: z.string(),
  after: z.string(),
})
export type Delta = z.infer<typeof delta>
export const revision = z.object({
  id: z.string(),
  ts: z.string(),
  actor: z.string(),
  path: z.string(),
  before: z.string(),
  after: z.string(),
  previous: z.string(),
  version: z.string(),
  event: z.number().optional(),
  attempt: z.string().optional(),
})
export const board = z.object({
  entries: z.array(z.object({ path: z.string(), source: z.string() })),
  models: z.array(
    z.object({
      id: z.string(),
      host: z.string(),
      name: z.string(),
      version: z.string(),
    })
  ),
  working: z.boolean(),
  checking: z.boolean(),
  versions: z.number(),
  candidate: z.object({
    stage: z.string(),
    stale: z.boolean(),
    token: z.string(),
    changes: z.array(delta),
  }),
  drafts: z.array(
    z.object({
      name: z.string(),
      problem: z.string().optional(),
      token: z.string(),
      changes: z.array(delta),
    })
  ),
  edits: z.array(revision),
  metrics: z.array(
    z.object({
      ts: z.string(),
      result: z.string().optional(),
      event: z.number().optional(),
      attempt: z.string().optional(),
      model: z.string().optional(),
      paths: z.array(z.string()).nullish(),
      new_notes: z.array(z.string()).nullish(),
      hold: z.string().optional(),
      reason: z.string().optional(),
      run_error: z.string().optional(),
      failures: z.array(z.string()).nullish(),
    })
  ),
  decisions: z.array(
    z.object({
      ts: z.string(),
      draft: z.string(),
      decision: z.string(),
      reason: z.string().optional(),
    })
  ),
  runs: z.array(
    z.object({
      event: z.number(),
      profile: z.string(),
      trigger: z.string(),
      state: z.string(),
      run: z.string(),
      ts: z.string(),
      error: z.string(),
    })
  ),
})
export type Board = z.infer<typeof board>
export const documentSchema = z.object({
  path: z.string(),
  body: z.string(),
  version: z.string(),
  base: z.string(),
  writable: z.boolean(),
  evidence: z
    .array(z.object({ source: z.string(), quote: z.string() }))
    .nullable(),
  history: z.array(z.string()),
})
export const usesSchema = z.array(
  z.object({
    run: z.string(),
    ts: z.string(),
    kind: z.string(),
    version: z.string(),
  })
)
export function entryLink(path: string) {
  return "#evolution?" + new URLSearchParams({ path })
}
export const labels: Record<string, string> = {
  pending: "已排队",
  running: "整理中",
  processing: "整理中",
  checking: "检查候选",
  done: "完成",
  skipped: "已跳过",
  held: "待审核",
  failed: "失败",
  rejected: "检查未通过",
  unchanged: "无变化",
  promoted: "已生效",
  promote: "人工提升",
  discard: "人工丢弃",
  rollback: "人工回滚",
  discarded: "已丢弃",
  approve: "已批准",
  reject: "已驳回",
  dead: "失败待处理",
  interrupted: "已中断",
}
export const when = (ts: string) => new Date(ts).toLocaleString("zh-CN")
export function requestKey() {
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (x) =>
    x.toString(16).padStart(2, "0")
  ).join("")
}
