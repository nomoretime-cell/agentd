import { useQuery } from "@tanstack/react-query"
import { z } from "zod"

export async function request<T>(
  path: string,
  schema: z.ZodType<T>,
  options: RequestInit = {},
  readError?: (body: unknown) => string | undefined
): Promise<T> {
  let response: Response
  try {
    response = await fetch("/v1/" + path, {
      ...options,
      redirect: "error",
      headers: {
        "X-Agentd-Workbench": "1",
        ...(options.body && { "Content-Type": "application/json" }),
      },
    })
  } catch (error) {
    if (options.signal?.aborted) throw error
    throw new Error("连接中断，请刷新核对服务端状态。", { cause: error })
  }
  if (!response.ok) {
    const errors: Record<number, string> = {
      401: "当前服务不支持工作台请求，请确认服务版本。",
      403: "当前访问地址或操作未获授权。",
      404: "找不到这条记录。",
      409: "状态已经变化，请查看最新记录。",
    }
    const detail =
      readError && readError(await response.json().catch(() => null))
    throw new Error(
      detail ??
        errors[response.status] ??
        `服务请求失败（HTTP ${response.status}）`
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
const statusSchema = z.object({
  active: z.boolean(),
  paused: z.boolean(),
  executor: z.string(),
  model: z.string(),
  im_provider: z.enum(["wecom", "dingtalk", "none"]),
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

export function useServiceStatus() {
  return useQuery({
    queryKey: ["status"],
    queryFn: ({ signal }) => request("status", statusSchema, { signal }),
    refetchInterval: 5000,
  })
}
