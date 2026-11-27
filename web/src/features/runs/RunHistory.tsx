import { serverTime } from "@/lib/server-time"
import { useInfiniteQuery, useQuery } from "@tanstack/react-query"
import { Dialog } from "radix-ui"
import { Search, X } from "lucide-react"
import { z } from "zod"
import { request } from "@/api"

import { RunEvidence, evidenceSchema } from "./RunEvidence"
import { ConversationList, DeliveryList, MessageDetail } from "./TraceBrowser"
import { deliveryStatus } from "@/lib/status"
import { conversationLabel, recordViews } from "@/lib/work-records"

const scope = z.object({
  ts: z.string(),
  status: z.string(),
  role: z.string(),
  principal: z.string(),
  conversation: z.string(),
})
const pageSchema = z.object({
  from: z.string(),
  to: z.string(),
  timezone: z.string(),
  next: z.string(),
  incomplete: z.boolean(),
  items: z.array(scope.extend({ id: z.string(), excerpt: z.string() })),
})
const runSchema = scope.extend({
  reply_ids: z.array(z.string()).nullable(),
  evidence: evidenceSchema.nullable(),
  channel: z.string(),
  calls: z
    .array(
      z.object({
        id: z.string(),
        model: z.string(),
        executor: z.string(),
        input: z.number().nullable(),
        output: z.number().nullable(),
        total: z.number().nullable(),
      })
    )
    .nullable(),
  answer: z.string(),
  error: z.string(),
  pending_action: z.string(),
  inputs: z
    .array(
      z.object({
        event: z.number(),
        kind: z.string(),
        name: z.string(),
        text: z.string(),
      })
    )
    .nullable(),
  notifications: z
    .array(
      z.object({
        key: z.string(),
        target: z.string(),
        content: z.string(),
        delivery_id: z.number(),
        delivery_status: z.string(),
      })
    )
    .nullable(),
})
const states: Record<string, string> = {
  running: "处理中",
  interrupted: "重启时中断",
  done: "已完成",
  failed: "处理失败",
  commit_failed: "提交失败",
  aborted: "已中断",
  skipped: "已跳过",
}
function runLink(params: URLSearchParams) {
  return params.size ? "#runs?" + params.toString() : "#runs"
}
function viewLink(view: string) {
  return view === "runs" ? "#runs" : "#runs?view=" + view
}

export function RunHistory({ search }: { search: URLSearchParams }) {
  const params = new URLSearchParams(search)
  const id = params.get("run")
  const viewingAction = params.has("action")
  params.delete("action")
  params.delete("run")
  const message = params.get("message")
  params.delete("message")
  const view = params.get("view") || "runs"
  const queryParams = new URLSearchParams(params)
  queryParams.delete("view")
  const filter = queryParams.toString()
  const list = useInfiniteQuery({
    queryKey: ["runs", filter],
    enabled: view === "runs",
    initialPageParam: "",
    queryFn: ({ pageParam, signal }) =>
      request("runs?" + (pageParam || filter), pageSchema, { signal }),
    getNextPageParam: (last) => {
      if (!last.next) return undefined
      const next = new URLSearchParams(filter)
      next.set("from", last.from)
      next.set("to", last.to)
      next.set("cursor", last.next)
      return next.toString()
    },
    staleTime: 60000,
  })
  const pages = list.data?.pages
  const first = pages?.[0]
  const rows = pages?.flatMap((p) => p.items) ?? []
  return (
    <section className="panel history-panel" aria-label="工作记录">
      <nav className="history-tabs" aria-label="工作记录视图">
        {Object.entries(recordViews).map(([key, title]) => (
          <a
            key={key}
            aria-current={view === key ? "page" : undefined}
            href={viewLink(key)}
          >
            {title}
          </a>
        ))}
      </nav>
      {view !== "deliveries" && (
        <form
          className="history-filters"
          key={filter}
          onSubmit={(e) => {
            e.preventDefault()
            const next = new URLSearchParams()
            for (const [k, v] of new FormData(e.currentTarget))
              if (typeof v === "string" && v.trim()) next.set(k, v.trim())
            const from = next.get("from")
            const to = next.get("to")
            if (from && to) {
              const days = (Date.parse(to) - Date.parse(from)) / 86400000
              if (days < 0 || days > 30) {
                const field = e.currentTarget.elements.namedItem(
                  "to"
                ) as HTMLInputElement
                field.setCustomValidity(
                  "请选择先后有序且不超过 31 天的日期范围。"
                )
                field.reportValidity()
                field.setCustomValidity("")
                return
              }
            }
            if (view !== "runs") next.set("view", view)
            location.assign(runLink(next))
          }}
        >
          <label>
            关键词
            <input
              name="q"
              defaultValue={params.get("q") || ""}
              placeholder="输入、回复或通知"
              type="search"
            />
          </label>
          <label>
            处理状态
            <select name="status" defaultValue={params.get("status") || ""}>
              <option value="">全部状态</option>
              {Object.entries(states).map(([v, t]) => (
                <option key={v} value={v}>
                  {t}
                </option>
              ))}
            </select>
          </label>
          <label>
            开始日期
            <input
              name="from"
              type="date"
              defaultValue={params.get("from") || ""}
            />
          </label>
          <label>
            结束日期
            <input
              name="to"
              type="date"
              defaultValue={params.get("to") || ""}
            />
          </label>
          <button className="outline-btn" type="submit">
            <Search size={15} />
            查询
          </button>
          {filter && <a href={viewLink(view)}>清除筛选</a>}
          <details
            className="scope-filters"
            open={[
              "role",
              "principal",
              "conversation",
              "channel",
              "flag",
              "match_role",
            ].some((key) => params.has(key))}
          >
            <summary>更多筛选</summary>
            <div>
              {" "}
              <label>
                对话 ID
                <input
                  name="conversation"
                  defaultValue={
                    params.get("conversation") ??
                    params.get("match_conversation") ??
                    ""
                  }
                  placeholder="所有对话"
                />
              </label>
              {[
                ["role", "角色"],
                ["principal", "服务对象"],
              ].map(([key, label]) => (
                <label key={key}>
                  {label}
                  <input
                    name={key}
                    defaultValue={
                      params.get(key) ?? params.get("match_" + key) ?? ""
                    }
                  />
                </label>
              ))}
              <label>
                渠道
                <select
                  name="channel"
                  defaultValue={params.get("channel") || ""}
                >
                  <option value="">全部渠道</option>
                  <option value="local">本地</option>
                  <option value="web">网页对话</option>
                  <option value="wecom">企业微信</option>
                  <option value="dingtalk">钉钉</option>
                </select>
              </label>
              <label>
                特殊情况
                <select name="flag" defaultValue={params.get("flag") || ""}>
                  <option value="">全部记录</option>
                  <option value="approval">有审批</option>
                  <option value="error">有异常</option>
                </select>
              </label>
            </div>
          </details>
        </form>
      )}
      {view === "conversations" ? (
        <ConversationList filter={filter} />
      ) : view === "deliveries" ? (
        <DeliveryList />
      ) : (
        <>
          <div className="section-head">
            <div>
              <h2>{recordViews.runs}</h2>
              <p className="muted">
                {first
                  ? first.from + " 至 " + first.to + " · " + first.timezone
                  : "默认显示最近 7 天，可选择最多 31 天"}
              </p>
            </div>
            <button
              className="icon-button"
              onClick={() => void list.refetch()}
              disabled={list.isFetching}
            >
              刷新记录
            </button>
          </div>
          {list.isPending && (
            <p className="empty" role="status">
              正在读取历史记录…
            </p>
          )}
          {list.error && <p role="alert">{list.error.message}</p>}
          {pages?.some((p) => p.incomplete) && (
            <p className="history-note">
              部分历史行不完整或超过读取上限，已略过。
            </p>
          )}
          {first && !rows.length && (
            <p className="empty">
              {list.hasNextPage
                ? "本页扫描范围内没有匹配记录"
                : "所选范围内没有匹配记录"}
            </p>
          )}
          {rows.length > 0 && (
            <ul className="history-list">
              {rows.map((row) => {
                const next = new URLSearchParams(filter)
                next.set("run", row.id)
                return (
                  <li key={row.id}>
                    <a data-run-id={row.id} href={runLink(next)}>
                      <span className="history-copy">
                        <strong>{row.excerpt || "查看任务处理记录"}</strong>
                        <small>
                          {serverTime(row.ts)} ·{" "}
                          {conversationLabel(row.conversation)}
                        </small>
                      </span>
                      <span className="pill">
                        {states[row.status] || row.status || "历史未记录"}
                      </span>
                    </a>
                  </li>
                )
              })}
            </ul>
          )}
          {list.hasNextPage && (
            <div className="pagination">
              <button
                disabled={list.isFetching}
                onClick={() => void list.fetchNextPage()}
              >
                继续读取
              </button>
            </div>
          )}
        </>
      )}
      {message && !viewingAction && (
        <MessageDetail key={message} id={message} params={search} />
      )}
      {id && !message && !viewingAction && (
        <RunDetail key={id} id={id} filters={params} />
      )}
    </section>
  )
}

function RunDetail({ id, filters }: { id: string; filters: URLSearchParams }) {
  const detail = useQuery<z.infer<typeof runSchema>>({
    queryKey: ["run", id],
    refetchInterval: (q) => (q.state.data?.status === "running" ? 2000 : false),
    queryFn: ({ signal }) =>
      request("runs/" + encodeURIComponent(id), runSchema, { signal }),
  })
  const r = detail.data
  const messageLink = (message: string) => {
    const next = new URLSearchParams(filters)
    next.set("run", id)
    next.set("message", message)
    return runLink(next)
  }
  const byConversation = new URLSearchParams(filters)
  byConversation.delete("q")
  byConversation.delete("status")
  byConversation.delete("view")
  for (const key of ["attempt", "event", "flag", "channel"])
    byConversation.delete(key)
  if (r)
    for (const key of ["role", "principal", "conversation"] as const) {
      byConversation.delete(key)
      byConversation.set("match_" + key, r[key])
    }
  const actionLink = new URLSearchParams(filters)
  actionLink.set("run", id)
  if (r?.pending_action) actionLink.set("action", r.pending_action)
  return (
    <Dialog.Root
      open
      onOpenChange={(open) => {
        if (!open) location.replace(runLink(filters))
      }}
    >
      <Dialog.Portal>
        <Dialog.Overlay className="scrim" />
        <Dialog.Content
          className="sheet"
          aria-describedby={undefined}
          onCloseAutoFocus={(event) => {
            const row = document.querySelector<HTMLAnchorElement>(
              '[data-run-id="' + CSS.escape(id) + '"]'
            )
            if (row) {
              event.preventDefault()
              row.focus({ preventScroll: true })
            }
          }}
        >
          <div className="section-head">
            <Dialog.Title>任务处理详情</Dialog.Title>
            <Dialog.Close className="icon-button" aria-label="关闭任务处理详情">
              <X size={17} />
            </Dialog.Close>
          </div>
          {detail.isPending && <p role="status">正在读取记录…</p>}
          {detail.error && (
            <p role="alert">
              {detail.error.message}{" "}
              <button onClick={() => void detail.refetch()}>重试</button>
            </p>
          )}
          {r && (
            <>
              <section>
                <h3>{states[r.status] || r.status || "状态未记录"}</h3>
                <p>
                  {serverTime(r.ts)} · {conversationLabel(r.conversation)}
                </p>
                <code>{id}</code>
                <p>
                  角色 {r.role || "历史未记录"} · 服务对象{" "}
                  {r.principal || "历史未记录"}
                </p>
                {r.channel === "web" && (
                  <p>
                    <a
                      className="history-link"
                      href={
                        "#chat?" + new URLSearchParams({ id: r.conversation })
                      }
                    >
                      继续这段对话
                    </a>
                  </p>
                )}
                {r.conversation && (
                  <a className="history-link" href={runLink(byConversation)}>
                    查看同一对话的记录
                  </a>
                )}
              </section>
              <section>
                <h3>本次处理的输入</h3>
                {r.inputs?.length ? (
                  r.inputs.map((i) => (
                    <div key={i.event}>
                      <p>
                        事件 #{i.event} · {i.kind} {i.name}
                      </p>
                      <pre>{i.text || "没有记录输入正文"}</pre>
                    </div>
                  ))
                ) : (
                  <p>历史未记录</p>
                )}
              </section>
              <section>
                <h3>记录中的回复</h3>
                <pre>{r.answer || "历史未记录回复正文"}</pre>
                {r.reply_ids?.map((reply) => (
                  <p key={reply}>
                    <a className="history-link" href={messageLink(reply)}>
                      查看完整回复 · {reply}
                    </a>
                  </p>
                ))}
              </section>
              <RunEvidence
                data={r.evidence}
                answer={[
                  r.answer,
                  ...(r.notifications ?? [])
                    .filter((n) => r.reply_ids?.includes(n.key))
                    .map((n) => n.content),
                ].join("\n")}
              />
              {r.calls?.some((c) => c.executor === "opencode") && (
                <p className="history-note">
                  OpenCode 仅展示宿主传入的上下文及执行器报告的步骤。
                </p>
              )}
              {r.error && (
                <section>
                  <h3>处理错误</h3>
                  <pre>{r.error}</pre>
                </section>
              )}
              {r.pending_action && (
                <section>
                  <h3>关联审批</h3>
                  <a className="history-link" href={runLink(actionLink)}>
                    查看动作 {r.pending_action}
                  </a>
                </section>
              )}
              <section>
                <h3>关联消息与发送回执</h3>
                {r.notifications?.length ? (
                  r.notifications.map((n, i) => (
                    <div key={i}>
                      <p>
                        {n.target} ·{" "}
                        {deliveryStatus[n.delivery_status] ||
                          n.delivery_status ||
                          "没有对应的发送回执"}{" "}
                        {n.delivery_id > 0 && "#" + n.delivery_id}
                      </p>
                      <pre>{n.content}</pre>
                      {n.delivery_id > 0 && (
                        <a
                          className="history-link"
                          href={messageLink("delivery:" + n.delivery_id)}
                        >
                          查看发送回执
                        </a>
                      )}
                    </div>
                  ))
                ) : (
                  <p>本次处理没有关联的消息发送记录</p>
                )}
              </section>
              <section>
                <h3>模型调用</h3>
                {r.calls?.length ? (
                  r.calls.map((c) => (
                    <p key={c.id}>
                      {c.model} · {c.executor}
                      <br />
                      输入 {c.input ?? "未报告"} · 输出 {c.output ?? "未报告"} ·
                      总量 {c.total ?? "未报告"}
                    </p>
                  ))
                ) : (
                  <p>
                    {r.calls ? "本次处理未记录模型调用" : "历史未采集调用用量"}
                  </p>
                )}
              </section>
              <p className="muted">
                历史正文可能已截断，本页仅展示已留存的上下文。
              </p>
            </>
          )}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
