import { z } from "zod"

export async function request<T>(
  token: string,
  path: string,
  schema: z.ZodType<T>,
  options: RequestInit = {}
): Promise<T> {
  let response: Response
  try {
    response = await fetch("/v1/" + path, {
      ...options,
      redirect: "error",
      headers: {
        ...(token && { Authorization: "Bearer " + token }),
        ...(options.body && { "Content-Type": "application/json" }),
      },
    })
  } catch (error) {
    if (options.signal?.aborted) throw error
    throw new Error("连接中断，请刷新核对服务端状态。", { cause: error })
  }
  if (!response.ok) {
    const errors: Record<number, string> = {
      401: "访问令牌无效，请重新解锁。",
      403: "当前访问地址或操作未获授权。",
      404: "找不到这条记录。",
      409: "状态已经变化，请查看最新记录。",
    }
    throw new Error(
      errors[response.status] ?? `服务请求失败（HTTP ${response.status}）`
    )
  }
  const parsed = schema.safeParse(await response.json())
  if (!parsed.success) throw new Error("服务返回的数据格式与工作台不兼容。")
  return parsed.data
}

const channel = z.object({
  enabled: z.boolean(),
  connected: z.boolean(),
  discovery_only: z.boolean(),
})
export const statusSchema = z.object({
  active: z.boolean(),
  paused: z.boolean(),
  executor: z.string(),
  model: z.string(),
  im_provider: z.enum(["wecom", "dingtalk"]),
  wecom: channel,
  dingtalk: channel,
  counts: z.object({
    inbox: z.number(),
    timers: z.number(),
    actions: z.number(),
    outbox: z.number(),
  }),
})
export type ServiceStatus = z.infer<typeof statusSchema>
