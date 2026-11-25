import { z } from "zod"
import { actionStatus, deliveryStatus, label } from "@/lib/status"
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
  limit: z.number().int().positive(),
})
const otherStatus: Record<string, string> = {
  dead: "死信",
  skill: "技能草稿",
  identity: "规则草稿",
}
// Each queue shows the wording of the record it lists.
export function itemStatus(kind: Kind, code: string) {
  const names =
    kind === "actions"
      ? actionStatus
      : kind === "outbox"
        ? deliveryStatus
        : otherStatus
  return label(names, code)
}
