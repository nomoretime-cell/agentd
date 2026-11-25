import { useState } from "react"
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query"
import { z } from "zod"
import { request } from "@/api"
import { Button } from "@/components/ui/button"
import {
  documentSchema,
  entryLink,
  usesSchema,
  revision,
  when,
  type Board,
} from "./data"
import { VersionDiff } from "./VersionDiff"
export function MemoryDocument({
  path,
  edits,
}: {
  path: string
  edits: Board["edits"]
}) {
  const client = useQueryClient()
  const doc = useQuery({
    queryKey: ["evolution", "document", path],
    queryFn: ({ signal }) =>
      request(
        "evolution/document?" + new URLSearchParams({ path }),
        documentSchema,
        { signal }
      ),
    refetchInterval: 4000,
  })
  const uses = useQuery({
    queryKey: ["evolution", "uses", path],
    queryFn: ({ signal }) =>
      request("evolution/uses?" + new URLSearchParams({ path }), usesSchema, {
        signal,
      }),
  })
  const [draft, setDraft] = useState<{
    body: string
    original: string
    version: string
    base: string
  } | null>(null)
  const [past, setPast] = useState("")
  const old = useQuery({
    queryKey: ["evolution", "old", path, past],
    enabled: !!past,
    queryFn: async ({ signal }) => {
      if (past.startsWith("edit:")) {
        const r = await request(
          "evolution/revisions/" + past.slice(5),
          revision,
          { signal }
        )
        return r.before
      }
      return (
        await request(
          "evolution/document?" + new URLSearchParams({ path, at: past }),
          documentSchema,
          { signal }
        )
      ).body
    },
  })
  const save = useMutation({
    mutationFn: async () => {
      if (!draft) throw Error("尚无修订")
      return request("evolution/edit", z.object({ path: z.string() }), {
        method: "POST",
        body: JSON.stringify({
          path,
          body: draft.body,
          version: draft.version,
          base: draft.base,
        }),
      })
    },
    onSuccess: (r) => {
      setDraft(null)
      setPast("")
      void client.invalidateQueries({ queryKey: ["evolution"] })
      location.hash = entryLink(r.path)
    },
    onError: () => {
      void doc.refetch()
    },
  })
  const d = doc.data
  if (!d)
    return (
      <section className="panel">
        <p role="status">{doc.error?.message || "正在读取条目…"}</p>
      </section>
    )
  const changed =
    draft !== null && (d.version !== draft.version || d.base !== draft.base)
  const edit = (body: string) =>
    setDraft({ body, original: d.body, version: d.version, base: d.base })
  return (
    <section className="panel">
      <h2>{path}</h2>
      <p className="muted">
        当前版本 <code>{d.version.slice(0, 12)}</code>
      </p>
      <pre className="memory-body" data-testid="memory-body">
        {d.body}
      </pre>
      {(d.evidence ?? []).map((ev, i) => {
        const session = /^sessions\/(\d{4}-\d{2}-\d{2})\.jsonl(?:@.*)?$/.exec(
          ev.source
        )
        const kd = /^knowledge\/md\/([a-f0-9]+)\.md(?:@.*)?$/.exec(ev.source)
        return (
          <blockquote key={i}>
            {ev.quote}
            <small>
              {kd ? (
                <a href={"#knowledge?doc=" + kd[1]}>{ev.source}</a>
              ) : session ? (
                <a
                  href={
                    "#runs?" +
                    new URLSearchParams({ from: session[1], to: session[1] })
                  }
                >
                  {ev.source} · 浏览当天运行
                </a>
              ) : (
                ev.source
              )}
            </small>
          </blockquote>
        )
      })}
      {d.writable && !draft && (
        <Button variant="outline" onClick={() => edit(d.body)}>
          {path.startsWith("skills/") ? "起草技能修订" : "编辑内容"}
        </Button>
      )}
      <label>
        历史版本
        <select value={past} onChange={(e) => setPast(e.target.value)}>
          <option value="">选择版本</option>
          {d.history.toReversed().map((v) => (
            <option key={v} value={v}>
              {v === "builtin" ? "发布内置版" : v}
            </option>
          ))}
          {edits
            .filter((r) => r.path === path && r.previous !== "absent")
            .map((r) => (
              <option key={r.id} value={"edit:" + r.id}>
                {when(r.ts)} 修改前 · {r.previous.slice(0, 10)}
              </option>
            ))}
        </select>
      </label>
      {past && old.data !== undefined && (
        <>
          <VersionDiff before={old.data} after={d.body} />
          {d.writable && (
            <Button
              disabled={save.isPending}
              variant="outline"
              onClick={() => edit(old.data)}
            >
              以此版本起草修订
            </Button>
          )}
        </>
      )}
      {draft && (
        <form
          onSubmit={(e) => {
            e.preventDefault()
            save.mutate()
          }}
        >
          <label>
            修订正文
            <textarea
              disabled={save.isPending}
              rows={12}
              value={draft.body}
              onChange={(e) => setDraft({ ...draft, body: e.target.value })}
            />
          </label>
          <h3>保存前对比</h3>
          <VersionDiff before={draft.original} after={draft.body} />
          {changed && (
            <p role="alert">
              服务器已有新版本。你的修订仍保留。
              <button
                type="button"
                className="history-link"
                onClick={() => edit(draft.body)}
              >
                对照最新版本重新审核
              </button>
            </p>
          )}
          <div className="evo-actions">
            <Button
              type="submit"
              disabled={
                save.isPending || changed || draft.body === draft.original
              }
            >
              {path.startsWith("skills") ? "保存待审草稿" : "保存记忆修订"}
            </Button>
            <Button
              type="button"
              variant="outline"
              disabled={save.isPending}
              onClick={() => setDraft(null)}
            >
              取消编辑
            </Button>
          </div>
        </form>
      )}
      {[doc.error, save.error, old.error, uses.error].map(
        (e, i) =>
          e && (
            <p key={i} role="alert">
              {e.message}
            </p>
          )
      )}
      <h3>近 30 天使用依据</h3>
      {!uses.data?.length && <p className="muted">暂无可关联的运行记录。</p>}
      {uses.data?.map((u) => (
        <p key={u.run + u.kind + u.version}>
          <a href={"#runs?run=" + encodeURIComponent(u.run)}>
            {when(u.ts)} ·{" "}
            {u.kind === "read"
              ? "工具读取"
              : u.kind === "hit"
                ? "检索命中"
                : "随请求提供"}{" "}
            · {u.version.slice(0, 12) || "版本未记录"}
          </a>
        </p>
      ))}
    </section>
  )
}
