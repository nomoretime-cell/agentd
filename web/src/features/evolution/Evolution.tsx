import { useEffect, useRef, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { z } from "zod"
import { requestKey, pendingKey } from "@/lib/request-key"
import { request } from "@/api"
import { Button } from "@/components/ui/button"
import { board, entryLink, labels, when, type Board, type Delta } from "./data"
import { MemoryDocument } from "./MemoryDocument"
import { VersionDiff } from "./VersionDiff"
import "./evolution.css"

// Group values stay as stored in bookmarked URLs; only their wording changes.
const groupLabels: Record<string, string> = { 需核对: "待核对" }

const reviewActions = {
  promote: "采用候选版本",
  discard: "丢弃候选版本",
  rollback: "回滚至上一版记忆",
  approve: "批准发布",
  reject: "驳回草稿",
}

function Changes({ changes }: { changes: Delta[] }) {
  return changes.map((c) => (
    <details key={c.path}>
      <summary>{c.path}</summary>
      <VersionDiff before={c.before} after={c.after} />
    </details>
  ))
}
function Review({
  data,
  draft,
  onDecided,
}: {
  data: Board
  draft?: string
  onDecided: (name: string, decision: string) => void
}) {
  // Follow the link once. A later poll may remove the draft while the owner is editing elsewhere.
  useEffect(() => {
    if (draft) document.getElementById("linked-draft")?.focus()
  }, [draft])
  const client = useQueryClient(),
    [reason, setReason] = useState<Record<string, string>>({})
  const action = useMutation({
    mutationFn: (body: {
      name: string
      decision: string
      token: string
      reason: string
    }) =>
      request("evolution/review", z.object({ decision: z.string() }), {
        method: "POST",
        body: JSON.stringify(body),
      }),
    // The page owns the URL and may ignore this if the owner has moved on.
    onSuccess: (_, req) => {
      if (req.name) onDecided(req.name, req.decision)
    },
    onSettled: () => {
      void client.invalidateQueries({ queryKey: ["evolution"] })
    },
  })
  function decide(
    name: string,
    decision: keyof typeof reviewActions,
    token: string
  ) {
    if (window.confirm("确认" + reviewActions[decision] + "？"))
      action.mutate({ name, decision, token, reason: reason[name] || "" })
  }
  const c = data.candidate
  return (
    <section className="panel">
      <h2>候选版本与草稿审核</h2>
      {c.stage ? (
        <>
          <p>
            {data.checking ? "检查候选版本" : labels[c.stage]}
            {c.stale ? " · 和当前基线不一致" : ""}
          </p>
          <Changes changes={c.changes} />
        </>
      ) : (
        <p className="muted">当前没有待审核的候选版本。</p>
      )}
      <div className="evo-actions">
        {c.stage === "held" && (
          <>
            <Button
              disabled={data.working || c.stale || action.isPending}
              onClick={() => decide("", "promote", c.token)}
            >
              {reviewActions.promote}
            </Button>
            <Button
              variant="outline"
              disabled={data.working || action.isPending}
              onClick={() => decide("", "discard", c.token)}
            >
              {reviewActions.discard}
            </Button>
          </>
        )}
        {data.versions > 0 && (
          <Button
            variant="outline"
            disabled={data.working || action.isPending}
            onClick={() => decide("", "rollback", c.token)}
          >
            {reviewActions.rollback}
          </Button>
        )}
      </div>
      {draft && !data.drafts.some((d) => d.name === draft) && (
        <p role="status" tabIndex={-1} id="linked-draft">
          草稿「{draft}」已不在待审列表中，可能已在其他窗口处理。
        </p>
      )}
      {data.drafts.map((d) => (
        <div className="evo-draft" key={d.name}>
          <h3 id={draft === d.name ? "linked-draft" : undefined} tabIndex={-1}>
            待审草稿 · {d.name}
          </h3>
          <Changes changes={d.changes} />
          {d.problem && <p role="alert">{d.problem}</p>}
          <label>
            驳回理由
            <input
              value={reason[d.name] || ""}
              onChange={(e) =>
                setReason({ ...reason, [d.name]: e.target.value })
              }
            />
          </label>
          <div className="evo-actions">
            <Button
              disabled={!!d.problem || action.isPending || data.working}
              onClick={() => decide(d.name, "approve", d.token)}
            >
              {reviewActions.approve}
            </Button>
            <Button
              variant="outline"
              disabled={
                !reason[d.name]?.trim() || action.isPending || data.working
              }
              onClick={() => decide(d.name, "reject", d.token)}
            >
              {reviewActions.reject}
            </Button>
          </div>
        </div>
      ))}
      {action.error && <p role="alert">{action.error.message}</p>}
    </section>
  )
}
function Dreaming({ data }: { data: Board }) {
  const client = useQueryClient(),
    [choice, setChoice] = useState(""),
    keys = useRef(new Map<string, string>()),
    [event, setEvent] = useState(0)
  const selected = data.models.find(
    (m) => m.id === (choice || data.models[0]?.id)
  )
  const send = useMutation({
    mutationFn: ({
      profile,
      version,
      key,
    }: {
      profile: string
      version: string
      key: string
      storageKey: string
    }) =>
      request("evolution/dream", z.object({ event: z.number() }), {
        method: "POST",
        body: JSON.stringify({ profile, version, key }),
      }),
    onSuccess: (r, submitted) => {
      if (pendingKey(submitted.storageKey) === submitted.key)
        pendingKey(submitted.storageKey, "")
      keys.current.delete(submitted.storageKey)
      setEvent(r.event)
      void client.invalidateQueries({ queryKey: ["evolution"] })
    },
  })
  function submit() {
    if (!selected || send.isPending) return
    const storageKey = "agentd-dream-" + selected.id + "-" + selected.version
    const key =
      keys.current.get(storageKey) || pendingKey(storageKey) || requestKey()
    keys.current.set(storageKey, key)
    pendingKey(storageKey, key)
    send.mutate({
      profile: selected.id,
      version: selected.version,
      key,
      storageKey,
    })
  }
  return (
    <section className="panel">
      <h2>记忆整理（Dreaming）</h2>
      {selected ? (
        <>
          <label>
            整理模型
            <select
              value={selected.id}
              disabled={send.isPending}
              onChange={(e) => setChoice(e.target.value)}
            >
              {data.models.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.id} · {m.name}
                </option>
              ))}
            </select>
          </label>
          <p className="muted">
            本次由 {selected.host}{" "}
            处理。可读取当前记忆、规则、知识资料，以及符合外发范围的所有者私聊和本地任务处理记录。
          </p>
        </>
      ) : (
        <p className="muted">
          服务器尚未配置可用于整理的模型。可在服务端启用 strong_model 或配置
          dream_models。
        </p>
      )}
      <Button
        disabled={
          !selected ||
          send.isPending ||
          data.working ||
          data.runs.some(
            (r) => r.state === "pending" || r.state === "running"
          ) ||
          (data.candidate.stage === "held" && !data.candidate.stale)
        }
        onClick={submit}
      >
        立即整理
      </Button>
      {event !== 0 && <p role="status">已提交整理任务 #{event}</p>}
      {send.error && <p role="alert">{send.error.message}</p>}
      {data.runs.map((r) => (
        <div className="evo-row" key={r.event}>
          <strong>
            #{r.event} · {r.trigger} · {r.profile}
          </strong>
          <span>
            {labels[r.state] ?? r.state} · {when(r.ts)}
          </span>
          {r.run && (
            <a href={"#runs?run=" + encodeURIComponent(r.run)}>查看处理依据</a>
          )}
          {r.error && <small>{r.error}</small>}
        </div>
      ))}
    </section>
  )
}
// Filters that a shared or refreshed link restores live in the URL; an empty
// source or group means all.
export type EvolutionFilters = { source: string; group: string; days: 7 | 30 }

export function Evolution({
  path,
  draft,
  filters,
  onFilter,
  onDecided,
}: {
  path: string
  draft?: string
  filters: EvolutionFilters
  onFilter: (next: Partial<EvolutionFilters>) => void
  onDecided: (name: string, decision: string) => void
}) {
  const q = useQuery({
    queryKey: ["evolution", "board"],
    queryFn: ({ signal }) => request("evolution", board, { signal }),
    refetchInterval: 3000,
  })
  const { days } = filters
  const tab = filters.source || "全部"
  const group = filters.group || "全部"
  // Opening an entry keeps the current filters.
  const link = (p: string) =>
    "#evolution?" +
    new URLSearchParams({
      path: p,
      ...(filters.source && { source: filters.source }),
      ...(filters.group && { group: filters.group }),
      ...(days !== 7 && { days: String(days) }),
    })
  const data = q.data
  if (!data)
    return <p role="status">{q.error?.message || "正在读取记忆与技能…"}</p>
  const history = [
    ...data.metrics.map((m, i) => ({
      id: "m" + i,
      ts: m.ts,
      group: ["failed", "rejected", "held"].includes(m.result ?? "")
        ? "需核对"
        : "记忆整理",
      title: labels[m.result ?? ""] || m.result || "整理",
      detail: [
        m.model,
        ...(m.paths ?? []),
        m.reason,
        m.hold,
        m.run_error,
        ...(m.failures ?? []),
      ]
        .filter(Boolean)
        .join(" · "),
      href: m.event
        ? m.attempt
          ? "#runs?run=" + m.event + "~" + m.attempt
          : "#runs?event=" + m.event
        : "",
    })),
    ...data.decisions.map((d, i) => ({
      id: "d" + i,
      ts: d.ts,
      group: "技能与规则",
      title: labels[d.decision] || d.decision,
      detail: d.draft + (d.reason ? " · " + d.reason : ""),
      href:
        d.decision === "approve"
          ? entryLink(
              d.draft === "identity"
                ? "IDENTITY.md"
                : "skills/" + d.draft + "/SKILL.md"
            )
          : "",
    })),
    ...data.edits.map((r) => ({
      id: r.id,
      ts: r.ts,
      group: "文件修订",
      title: r.actor === "owner" ? "人工修订" : "任务写入",
      detail: r.path + " · " + r.version.slice(0, 12),
      href:
        r.event && r.attempt
          ? "#runs?run=" + r.event + "~" + r.attempt
          : entryLink(
              r.path.startsWith("skills-drafts/") &&
                !data.entries.some((e) => e.path === r.path)
                ? r.path.replace("skills-drafts/", "skills/")
                : r.path
            ),
    })),
  ]
    .filter((r) => Date.parse(r.ts) >= q.dataUpdatedAt - days * 86400000)
    .sort((a, b) => b.ts.localeCompare(a.ts))
  return (
    <div className="evolution-page">
      {q.error && <p role="alert">{q.error.message}</p>}
      <section className="panel">
        <h2>生效条目与草稿</h2>
        <label>
          条目类型
          <select
            value={tab}
            onChange={(e) =>
              onFilter({
                source: e.target.value === "全部" ? "" : e.target.value,
              })
            }
          >
            {["全部", ...new Set(data.entries.map((e) => e.source))].map(
              (x) => (
                <option key={x}>{x}</option>
              )
            )}
          </select>
        </label>
        <ul>
          {data.entries
            .filter((e) => tab === "全部" || e.source === tab)
            .map((e) => (
              <li key={e.path}>
                <a
                  href={link(e.path)}
                  aria-current={path === e.path ? "page" : undefined}
                >
                  {e.path}
                </a>{" "}
                <small>{e.source}</small>
              </li>
            ))}
        </ul>
      </section>
      {path && <MemoryDocument key={path} path={path} edits={data.edits} />}
      <Dreaming data={data} />
      <Review data={data} draft={draft} onDecided={onDecided} />
      <section className="panel">
        <h2>变化记录</h2>
        <label>
          时间范围
          <select
            value={days}
            onChange={(e) =>
              onFilter({ days: e.target.value === "30" ? 30 : 7 })
            }
          >
            <option value={7}>近 7 天</option>
            <option value={30}>近 30 天</option>
          </select>
        </label>
        <div className="evo-actions">
          {["全部", "记忆整理", "技能与规则", "文件修订", "需核对"].map((g) => (
            <Button
              key={g}
              variant={g === group ? "default" : "outline"}
              aria-pressed={g === group}
              onClick={() => onFilter({ group: g === "全部" ? "" : g })}
            >
              {groupLabels[g] ?? g} ·{" "}
              {history.filter((r) => g === "全部" || r.group === g).length}
            </Button>
          ))}
        </div>
        {data.metrics
          .filter(
            (m) =>
              ["promoted", "promote"].includes(m.result ?? "") &&
              Date.parse(m.ts) >= q.dataUpdatedAt - days * 86400000 &&
              !!m.new_notes?.length
          )
          .map((m, i) => (
            <p key={i}>
              整理新增笔记 ·{" "}
              {m.new_notes?.map((p) => (
                <a key={p} href={entryLink(p)}>
                  {p}{" "}
                </a>
              ))}
            </p>
          ))}
        {!history.some((r) => group === "全部" || r.group === group) && (
          <p className="muted">所选范围内没有已记录的变化。</p>
        )}
        {history
          .filter((r) => group === "全部" || r.group === group)
          .map((r) => (
            <div className="evo-row" key={r.id}>
              <strong>{r.title}</strong>
              <small>{when(r.ts)}</small>
              <span>{r.detail}</span>
              {r.href && <a href={r.href}>查看详情</a>}
            </div>
          ))}
        <small>
          显示最近 100 次文件修订及各 80 条整理和审核回执。旧记录可能缺少来源。
        </small>
      </section>
    </div>
  )
}
