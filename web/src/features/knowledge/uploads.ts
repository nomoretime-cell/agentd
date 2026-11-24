import { z } from "zod"
export const doc = z.object({
  doc_id: z.string(),
  source: z.string(),
  title: z.string(),
  sha256: z.string(),
  bytes: z.number(),
  imported_at: z.string(),
})
export const library = z.object({
  documents: z
    .array(doc)
    .nullable()
    .transform((v) => v ?? []),
  importing: z.boolean(),
  last_import: z
    .array(
      z.object({
        source: z.string(),
        status: z.string(),
        error: z.string().optional(),
      })
    )
    .nullable(),
})
const fileResult = z.object({
  name: z.string(),
  status: z.string(),
  error: z.string().optional(),
  doc_id: z.string().optional(),
})
export const batch = z.object({
  id: z.string(),
  created: z.string(),
  files: z.array(fileResult),
})
export const uploads = z.object({
  batches: z.array(batch),
  native: z.array(z.string()),
  converted: z.array(z.string()),
  converter: z.boolean(),
  max_file: z.number(),
  max_text: z.number(),
  max_count: z.number(),
  max_total: z.number(),
})
export type Selection = {
  file: File
  mode: string
  target: string
  version: string
}
export const statuses: Record<string, string> = {
  processing: "服务器处理中",
  added: "已入库",
  updated: "已更新",
  unchanged: "未变化",
  failed: "失败",
  archived: "已归档",
}
export function key() {
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) =>
    b.toString(16).padStart(2, "0")
  ).join("")
}
export function transmit(
  id: string,
  rows: Selection[],
  signal: AbortSignal,
  onProgress: (n: number) => void
) {
  const data = new FormData()
  data.append(
    "selection",
    JSON.stringify(
      rows.map(({ file, mode, target, version }) => ({
        name: file.name,
        mode,
        target,
        version,
      }))
    )
  )
  for (const { file } of rows) data.append("file", file, "blob")
  return new Promise<void>((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    const abort = () => xhr.abort()
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) onProgress(Math.floor((e.loaded / e.total) * 100))
    }
    xhr.open("POST", "/v1/knowledge/uploads?key=" + id)
    xhr.setRequestHeader("X-Agentd-Workbench", "1")
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve()
        return
      }
      let message = "上传未成功，请核对批次记录。"
      try {
        const parsed = z
          .object({ error: z.string() })
          .safeParse(JSON.parse(xhr.responseText))
        if (parsed.success) message = parsed.data.error
      } catch {
        /* Non-JSON proxy response. */
      }
      reject(new Error(message))
    }
    xhr.onerror = () => reject(new Error("传输中断，请核对服务器回执后重试。"))
    xhr.onabort = () => reject(new Error("已取消传输。"))
    xhr.onloadend = () => signal.removeEventListener("abort", abort)
    signal.addEventListener("abort", abort, { once: true })
    xhr.send(data)
  })
}
