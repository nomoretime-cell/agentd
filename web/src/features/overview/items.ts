import { z } from "zod"
export const categories = {
  actions: "动作审批与核对",
  inbox: "异常事件",
  outbox: "异常投递",
  drafts: "待审草稿",
} as const
export type Kind = keyof typeof categories
export const itemSchema = z.object({
  id: z.string(),
  title: z.string(),
  status: z.string(),
  detail: z.string(),
})
export type Item = z.infer<typeof itemSchema>
export const workSchema = z.object({
  counts: z.object({
    actions: z.number(),
    inbox: z.number(),
    outbox: z.number(),
    drafts: z.number(),
  }),
  items: z.array(itemSchema),
})
export const statusNames: Record<string, string> = {
  pending_approval: "待审批",
  approved: "已批准",
  rejected: "已拒绝",
  executing: "执行中",
  succeeded: "已执行",
  failed: "失败",
  unknown: "待核对",
  dead: "死信",
  skill: "技能草稿",
  identity: "规则草稿",
}
