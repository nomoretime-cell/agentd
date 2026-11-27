import { useEffect, useRef, useState } from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { z } from "zod"
import { requestKey } from "@/lib/request-key"
import { Button } from "@/components/ui/button"
import { request } from "@/api"
import {
  doc,
  receiptFor,
  size,
  statuses,
  transmit,
  uploads,
  type Selection,
} from "./uploads"

// Uploading from this computer: selection, transfer and the batch receipt.
// It shares the uploads query with the recent-batches list.
export function UploadPanel({
  documents: list,
}: {
  documents: z.infer<typeof doc>[]
}) {
  const client = useQueryClient()
  const records = useQuery({
    queryKey: ["knowledge", "uploads"],
    queryFn: ({ signal }) => request("knowledge/uploads", uploads, { signal }),
  })
  const cfg = records.data
  const refresh = () => client.invalidateQueries({ queryKey: ["knowledge"] })
  const [rows, setRows] = useState<Selection[]>([]),
    [percent, setPercent] = useState<number | null>(null),
    [message, setMessage] = useState(""),
    [active, setActive] = useState("")
  const flight = useRef<AbortController | null>(null)
  useEffect(() => () => flight.current?.abort(), [])
  const current = useQuery({
    queryKey: ["knowledge", "receipt", active],
    queryFn: ({ signal }) =>
      request("knowledge/uploads/" + active, receiptFor(active), { signal }),
    enabled: !!active,
    refetchInterval: (q) =>
      q.state.status === "error" ||
      q.state.data?.files.every((f) => f.status !== "processing")
        ? false
        : 1000,
  })
  const submitted = !!current.data
  const busy =
    percent !== null ||
    (!!active &&
      !current.isError &&
      (current.isPending ||
        current.data?.files.some((f) => f.status === "processing")))
  function edit(next: Selection[]) {
    setRows(next)
    setMessage("")
    setActive("")
  }
  function add(files: File[]) {
    if (!files.length || busy) return
    edit([
      ...(submitted ? [] : rows),
      ...files.map((file) => ({
        file,
        mode: "match",
        target: "",
        version: "",
      })),
    ])
  }
  const formats = cfg
    ? [...cfg.native, ...(cfg.converter ? cfg.converted : [])]
    : []
  function problem(file: File) {
    const ext = /\.[^.]+$/.exec(file.name)?.[0].toLowerCase() ?? ""
    if (!cfg) return "正在读取上传限制"
    if (!formats.includes(ext))
      return cfg.converted.includes(ext) ? "服务器未配置转换器" : "格式不支持"
    if (file.size === 0) return "空文件"
    if (
      file.size > cfg.max_file ||
      (cfg.native.includes(ext) && file.size > cfg.max_text)
    )
      return "文件超过大小限制"
    return ""
  }
  const over =
    cfg &&
    (rows.length > cfg.max_count ||
      rows.reduce((n, r) => n + r.file.size, 0) > cfg.max_total)
  async function send() {
    if (flight.current || !rows.length) return
    const id = active || requestKey()
    const controller = new AbortController()
    flight.current = controller
    setPercent(0)
    setMessage("")
    try {
      const receipt = await transmit(id, rows, controller.signal, setPercent)
      client.setQueryData(["knowledge", "receipt", id], receipt)
    } catch (e) {
      setMessage(
        e instanceof Error ? e.message : "上传未成功，请核对上传结果。"
      )
    } finally {
      flight.current = null
      setPercent(null)
      setActive(id)
      void refresh()
    }
  }
  return (
    <section className="panel">
      <h2>上传文件</h2>
      <p className="muted">
        选择要上传的文件，原件将保存到服务器，入库后即可检索。
      </p>
      {cfg && (
        <p className="knowledge-limits">
          支持 {formats.join("、")}。单个原件 {size(cfg.max_file)}，转换后正文{" "}
          {size(cfg.max_text)}。每批最多 {cfg.max_count} 个，共{" "}
          {size(cfg.max_total)}。
          {!cfg.converter && " PDF 和 Office 文件需要服务器配置转换器。"}
        </p>
      )}
      <div
        className="knowledge-drop"
        onDragOver={(e) => e.preventDefault()}
        onDrop={(e) => {
          e.preventDefault()
          add(Array.from(e.dataTransfer.files))
        }}
      >
        <label>
          选择文件
          <input
            type="file"
            multiple
            accept={formats.join(",")}
            disabled={!cfg || busy}
            onChange={(e) => {
              add(Array.from(e.target.files ?? []))
              e.target.value = ""
            }}
          />
        </label>
        <small>也可拖入文件</small>
      </div>
      {!!rows.length && (
        <ul>
          {rows.map((r, i) => {
            const matches = list.filter(
              (d) =>
                d.source.startsWith("upload:") &&
                d.source.split("/").at(-1) === r.file.name
            )
            const result = current.data?.files[i]
            return (
              <li key={i}>
                <strong>{r.file.name}</strong>{" "}
                <small>{size(r.file.size)}</small> ·{" "}
                {result
                  ? (statuses[result.status] ?? result.status)
                  : percent !== null
                    ? percent === 100
                      ? "等待服务器接收"
                      : "传输中"
                    : problem(r.file) || "等待上传"}
                {result?.error && <p role="alert">{result.error}</p>}
                {!busy && !submitted && (
                  <>
                    <button
                      type="button"
                      className="outline-btn"
                      onClick={() => edit(rows.filter((_, j) => i !== j))}
                    >
                      移除
                    </button>
                    {(matches.length > 0 ||
                      rows
                        .slice(0, i)
                        .some((other) => other.file.name === r.file.name)) && (
                      <label>
                        同名文件处理
                        <select
                          value={r.mode === "update" ? r.target : r.mode}
                          onChange={(e) => {
                            const val = e.target.value,
                              d = matches.find((x) => x.doc_id === val)
                            edit(
                              rows.map((x, j) =>
                                j !== i
                                  ? x
                                  : {
                                      ...x,
                                      mode: d ? "update" : val,
                                      target: d?.doc_id ?? "",
                                      version: d?.sha256 ?? "",
                                    }
                              )
                            )
                          }}
                        >
                          <option value="match">
                            相同内容跳过，内容不同提示冲突
                          </option>
                          <option value="copy">另存为新文档</option>
                          {matches.map((d) => (
                            <option key={d.doc_id} value={d.doc_id}>
                              更新 {d.title}（{d.doc_id}）
                            </option>
                          ))}
                        </select>
                      </label>
                    )}
                  </>
                )}
              </li>
            )
          })}
        </ul>
      )}
      {over && (
        <p role="alert">本批文件数量或总大小超过限制，请移除部分文件。</p>
      )}
      {percent !== null && (
        <p role="status">
          本批传输 {percent}% <progress value={percent} max={100} />
        </p>
      )}
      {message && !submitted && <p role="alert">{message}</p>}
      <div className="knowledge-actions">
        {active && current.isError && (
          <Button
            variant="outline"
            disabled={current.isFetching || percent !== null}
            onClick={() => void current.refetch()}
          >
            核对上传结果
          </Button>
        )}
        {(!submitted || current.isError) && (
          <Button
            disabled={
              !cfg ||
              !rows.length ||
              !!over ||
              busy ||
              current.isFetching ||
              rows.some((r) => problem(r.file))
            }
            onClick={() => void send()}
          >
            {active ? "按原批次重试" : "开始上传"}
          </Button>
        )}
        {percent !== null && (
          <Button variant="outline" onClick={() => flight.current?.abort()}>
            取消传输
          </Button>
        )}
        {submitted &&
          !busy &&
          current.data?.files.some((f) => f.status === "failed") && (
            <Button
              onClick={() =>
                edit(
                  rows
                    .filter(
                      (_, i) => current.data?.files[i]?.status === "failed"
                    )
                    // A failed update may have met a newer version; the
                    // owner chooses the target again instead of reusing it.
                    .map((r) =>
                      r.mode === "update"
                        ? { ...r, mode: "match", target: "", version: "" }
                        : r
                    )
                )
              }
            >
              重试失败项
            </Button>
          )}
      </div>
    </section>
  )
}
