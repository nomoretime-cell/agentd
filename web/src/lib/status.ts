// One wording per stored state, shared by every page that shows it. Codes
// without a label are shown as received, so a new server state stays visible.
export const actionStatus: Record<string, string> = {
  pending_approval: "待审批",
  approved: "已批准",
  rejected: "已拒绝",
  executing: "执行中",
  succeeded: "已执行",
  failed: "执行失败",
  unknown: "待核对",
}

export const deliveryStatus: Record<string, string> = {
  pending: "待发送",
  sending: "发送中",
  sent: "已确认发送",
  committed: "本地已提交",
  failed: "发送失败",
  unknown: "待核对",
}

export function label(names: Record<string, string>, code: string) {
  return names[code] ?? code
}

// A receipt records who confirmed the send. Older records may omit its source.
export function confirmationSource(source: string) {
  if (source === "owner") return "人工核对"
  if (source === "adapter") return "系统确认"
  return source || "确认来源未记录"
}
