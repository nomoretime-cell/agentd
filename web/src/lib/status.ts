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
  pending: "待投递",
  sending: "投递中",
  sent: "投递已确认",
  failed: "投递失败",
  unknown: "待核对",
}

export function label(names: Record<string, string>, code: string) {
  return names[code] ?? code
}
