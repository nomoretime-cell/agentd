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
  files: z.array(fileResult).min(1),
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
export function receiptFor(id: string) {
  return batch.refine((receipt) => receipt.id === id, "批次编号不匹配")
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
  return new Promise<z.infer<typeof batch>>((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    const abort = () => xhr.abort()
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) onProgress(Math.floor((e.loaded / e.total) * 100))
    }
    xhr.open("POST", "/v1/knowledge/uploads?key=" + id)
    xhr.setRequestHeader("X-Agentd-Workbench", "1")
    xhr.responseType = "json"
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        const parsed = receiptFor(id).safeParse(xhr.response)
        if (parsed.success) resolve(parsed.data)
        else reject(new Error("服务器未返回有效的上传结果。"))
        return
      }
      const error = z.object({ error: z.string() }).safeParse(xhr.response)
      reject(
        new Error(
          error.success ? error.data.error : "上传未成功，请核对上传结果。"
        )
      )
    }
    xhr.onerror = () => reject(new Error("传输中断，请核对上传结果后重试。"))
    xhr.onabort = () => reject(new Error("已取消传输。"))
    xhr.onloadend = () => signal.removeEventListener("abort", abort)
    signal.addEventListener("abort", abort, { once: true })
    xhr.send(data)
  })
}

export function size(n: number) {
  return n < 1024
    ? n + " B"
    : n < 1 << 20
      ? Math.ceil(n / 1024) + " KiB"
      : (n / (1 << 20)).toFixed(1) + " MiB"
}
