import { useEffect, useRef, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { z } from "zod"
import { request } from "@/api"
import { Button } from "@/components/ui/button"
import {
  batch,
  doc,
  key,
  library,
  statuses,
  transmit,
  uploads,
  type Selection,
} from "./uploads"
import "./knowledge.css"
const searchResult = z.object({
  hits: z.array(z.object({ path: z.string(), snippet: z.string() })),
  truncated: z.boolean().optional(),
})
function size(n: number) {
  return n < 1024
    ? n + " B"
    : n < 1 << 20
      ? Math.ceil(n / 1024) + " KiB"
      : (n / (1 << 20)).toFixed(1) + " MiB"
}
export function Knowledge() {
  const client = useQueryClient()
  const docs = useQuery({
    queryKey: ["knowledge", "library"],
    queryFn: ({ signal }) => request("knowledge", library, { signal }),
    refetchInterval: 3000,
  })
  const records = useQuery({
    queryKey: ["knowledge", "uploads"],
    queryFn: ({ signal }) => request("knowledge/uploads", uploads, { signal }),
    refetchInterval: (q) =>
      q.state.data?.batches.some((b) =>
        b.files.some((f) => f.status === "processing")
      )
        ? 1500
        : 15000,
  })
  const cfg = records.data
  const [rows, setRows] = useState<Selection[]>([]),
    [percent, setPercent] = useState<number | null>(null),
    [message, setMessage] = useState(""),
    [active, setActive] = useState(""),
    [submitted, setSubmitted] = useState(false)
  const flight = useRef<AbortController | null>(null),
    attempt = useRef("")
  useEffect(() => () => flight.current?.abort(), [])
  const current = useQuery({
    queryKey: ["knowledge", "receipt", active],
    queryFn: ({ signal }) =>
      request("knowledge/uploads/" + active, batch, { signal }),
    enabled: !!active,
    refetchInterval: (q) =>
      q.state.data?.files.every((f) => f.status !== "processing")
        ? false
        : 1000,
  })
  const [text, setText] = useState(""),
    [query, setQuery] = useState(""),
    [selected, setSelected] = useState(""),
    [path, setPath] = useState("")
  const search = useQuery({
    queryKey: ["knowledge", "search", query],
    enabled: !!query,
    queryFn: ({ signal }) =>
      request("knowledge/search?q=" + encodeURIComponent(query), searchResult, {
        signal,
      }),
  })
  const detail = useQuery({
    queryKey: ["knowledge", "doc", selected],
    enabled: !!selected,
    queryFn: ({ signal }) =>
      request(
        "knowledge/" + selected,
        z.object({ document: doc, body: z.string() }),
        { signal }
      ),
  })
  const refresh = () => client.invalidateQueries({ queryKey: ["knowledge"] })
  const manage = useMutation({
    mutationFn: ({ url, body }: { url: string; body: object }) =>
      request(url, z.object({}), {
        method: "POST",
        body: JSON.stringify(body),
      }),
    onSuccess: () => {
      setSelected("")
      void refresh()
    },
  })
  const list = docs.data?.documents ?? []
  const busy =
    percent !== null ||
    (!!active &&
      (!current.data ||
        current.data.files.some((f) => f.status === "processing")))
  function edit(next: Selection[]) {
    setRows(next)
    setMessage("")
    setActive("")
    setSubmitted(false)
    attempt.current = ""
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
    const id = attempt.current || key()
    attempt.current = id
    const controller = new AbortController()
    flight.current = controller
    setPercent(0)
    setMessage("")
    try {
      await transmit(id, rows, controller.signal, setPercent)
      setActive(id)
      setSubmitted(true)
    } catch (e) {
      try {
        const found = await request("knowledge/uploads/" + id, batch)
        client.setQueryData(["knowledge", "receipt", id], found)
        setActive(id)
        setSubmitted(true)
      } catch {
        setMessage(e instanceof Error ? e.message : "上传失败，可重新提交。")
      }
    } finally {
      flight.current = null
      setPercent(null)
      void refresh()
    }
  }
  const errors = [
    docs.error,
    records.error,
    current.error,
    search.error,
    detail.error,
    manage.error,
  ].filter(Boolean)
  return (
    <div className="knowledge-page">
      <section className="panel">
        <h2>上传文件</h2>
        <p className="muted">
          从这台电脑选择资料，原件将保存到服务器，入库后即可检索。
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
            从这台电脑上传
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
                          .some(
                            (other) => other.file.name === r.file.name
                          )) && (
                        <label>
                          同名处理
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
        {message && <p role="alert">{message}</p>}
        <div className="knowledge-actions">
          {!submitted && (
            <Button
              disabled={
                !cfg ||
                !rows.length ||
                !!over ||
                busy ||
                rows.some((r) => problem(r.file))
              }
              onClick={() => void send()}
            >
              开始上传
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
                    rows.filter(
                      (_, i) => current.data?.files[i]?.status === "failed"
                    )
                  )
                }
              >
                重试失败项
              </Button>
            )}
        </div>
      </section>
      {errors.map((err, i) => (
        <p key={i} role="alert">
          {err?.message}
        </p>
      ))}
      <section className="panel">
        <h2>已入库文档</h2>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            setQuery(text.trim())
            if (query && text.trim() === query) void search.refetch()
          }}
        >
          <label>
            检索正文
            <input
              type="search"
              value={text}
              onChange={(e) => setText(e.target.value)}
              placeholder="输入文档中的词句"
            />
          </label>
          <Button variant="outline">检索</Button>
        </form>
        {query && (
          <div aria-live="polite">
            {search.isPending
              ? "检索中…"
              : search.data?.hits.length === 0
                ? "没有匹配的正文"
                : null}
            {search.data?.hits.map((h) => (
              <p key={h.path + h.snippet}>
                <button
                  className="history-link"
                  onClick={() =>
                    setSelected(h.path.split("/").at(-1)!.replace(/\.md$/, ""))
                  }
                >
                  {h.snippet}
                </button>
              </p>
            ))}
            {search.data?.truncated && "仅显示部分匹配，请缩小检索范围。"}
          </div>
        )}
        {docs.isPending ? (
          <p>正在读取文档…</p>
        ) : !list.length ? (
          <p className="muted">还没有入库文档。</p>
        ) : (
          <ul>
            {list.map((d) => (
              <li key={d.doc_id}>
                <button
                  className="history-link"
                  onClick={() => setSelected(d.doc_id)}
                >
                  {d.title}
                </button>{" "}
                <small>
                  {d.source.split("/").at(-1)} · {size(d.bytes)}
                </small>
              </li>
            ))}
          </ul>
        )}
        {detail.data && (
          <article>
            <h3>{detail.data.document.title}</h3>
            <pre>{detail.data.body}</pre>
            <div className="knowledge-actions">
              <Button variant="outline" onClick={() => setSelected("")}>
                收起正文
              </Button>
              <Button
                variant="outline"
                disabled={manage.isPending}
                onClick={() => {
                  if (
                    window.confirm("归档后，此文档将移出检索范围。确认归档？")
                  )
                    manage.mutate({
                      url: "knowledge/" + selected + "/remove",
                      body: {},
                    })
                }}
              >
                归档文档
              </Button>
            </div>
          </article>
        )}
      </section>
      <section className="panel">
        <h2>最近上传</h2>
        {!cfg?.batches.length ? (
          <p className="muted">暂无已接收批次。</p>
        ) : (
          cfg.batches.map((b) => (
            <details key={b.id}>
              <summary>
                {new Date(b.created).toLocaleString("zh-CN")} · {b.files.length}{" "}
                个文件 ·{" "}
                {b.files.some((f) => f.status === "processing")
                  ? "处理中"
                  : b.files.some((f) => f.status === "failed")
                    ? "含失败项"
                    : "已完成"}
              </summary>
              <ul>
                {b.files.map((f, i) => (
                  <li key={i}>
                    {f.name} · {statuses[f.status]} {f.error}
                  </li>
                ))}
              </ul>
            </details>
          ))
        )}
      </section>
      <details className="panel">
        <summary>从服务器路径导入</summary>
        <p>
          文件或目录须位于服务器状态根目录的 data/knowledge/incoming/
          下。填写相对路径，整个目录填「.」。
        </p>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            manage.mutate({
              url: "knowledge/incoming",
              body: { path: path.trim() },
            })
          }}
        >
          <label>
            服务器相对路径
            <input value={path} onChange={(e) => setPath(e.target.value)} />
          </label>
          <Button
            disabled={!path.trim() || manage.isPending || docs.data?.importing}
          >
            导入服务器资料
          </Button>
        </form>
        {docs.data?.importing && <p role="status">正在导入资料…</p>}
        {docs.data?.last_import?.map((r, i) => (
          <p key={i}>
            {r.source} · {statuses[r.status]} {r.error}
          </p>
        ))}
      </details>
    </div>
  )
}
