export const recordViews = {
  runs: "任务处理记录",
  conversations: "按对话查看",
  deliveries: "消息发送记录",
} as const

const scopes: Record<string, string> = {
  local: "本地对话",
  unknown: "来源未知",
  maintenance: "记忆整理",
}

export function conversationLabel(value: string) {
  return Object.hasOwn(scopes, value) ? scopes[value] : value || "历史未记录"
}
