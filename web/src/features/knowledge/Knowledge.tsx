import { useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { z } from "zod"
import { request } from "@/api"
import { Button } from "@/components/ui/button"
import { doc, library, size, statuses, uploads } from "./uploads"
import { UploadPanel } from "./UploadPanel"
import "./knowledge.css"
import { viewerInstant } from "@/lib/server-time"
const searchResult = z.object({
  hits: z.array(z.object({ path: z.string(), snippet: z.string() })),
  truncated: z.boolean().optional(),
})
export function Knowledge({
  selected,
  query,
  onSelect,
  onSearch,
}: {
  selected: string
  query: string
  onSelect: (id: string) => void
  onSearch: (q: string) => void
}) {
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
  const [path, setPath] = useState("")
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
  const post = ({ url, body }: { url: string; body: object }) =>
    request(url, z.object({}), {
      method: "POST",
      body: JSON.stringify(body),
    })
  const manage = useMutation({
    mutationFn: post,
    onSuccess: () => {
      onSelect("")
      void refresh()
    },
  })
  // Importing from the server leaves the open document as it is.
  const importer = useMutation({
    mutationFn: post,
    onSuccess: () => void refresh(),
  })
  const list = docs.data?.documents ?? []
  const errors = [
    docs.error,
    records.error,
    search.error,
    detail.error,
    manage.error,
    importer.error,
  ].filter(Boolean)
  return (
    <div className="knowledge-page">
      <UploadPanel documents={list} />
      {errors.map((err, i) => (
        <p key={i} role="alert">
          {err?.message}
        </p>
      ))}
      <section className="panel">
        <h2>已入库文档</h2>
        <form
          key={query}
          onSubmit={(e) => {
            e.preventDefault()
            const text = String(
              new FormData(e.currentTarget).get("q") ?? ""
            ).trim()
            if (text === query && query) void search.refetch()
            else onSearch(text)
          }}
        >
          <label>
            检索正文
            <input
              type="search"
              name="q"
              defaultValue={query}
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
                    onSelect(h.path.split("/").at(-1)!.replace(/\.md$/, ""))
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
                  onClick={() => onSelect(d.doc_id)}
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
            <small>入库版本 SHA-256 · {detail.data.document.sha256}</small>
            <pre data-testid="document-body">{detail.data.body}</pre>
            <div className="knowledge-actions">
              <Button variant="outline" onClick={() => onSelect("")}>
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
                {viewerInstant(b.created)} · {b.files.length} 个文件 ·{" "}
                {b.files.some((f) => f.status === "processing")
                  ? "处理中"
                  : b.files.some((f) => f.status === "failed")
                    ? "含失败项"
                    : "已完成"}
              </summary>
              <ul>
                {b.files.map((f, i) => (
                  <li key={i}>
                    {f.name} · {statuses[f.status] ?? f.status} {f.error}
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
            importer.mutate({
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
            disabled={
              !path.trim() || importer.isPending || docs.data?.importing
            }
          >
            导入服务器资料
          </Button>
        </form>
        {docs.data?.importing && <p role="status">正在导入资料…</p>}
        {docs.data?.last_import?.map((r, i) => (
          <p key={i}>
            {r.source} · {statuses[r.status] ?? r.status} {r.error}
          </p>
        ))}
      </details>
    </div>
  )
}
