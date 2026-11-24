import { useLayoutEffect, useRef, useState } from "react"
import {
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query"
import { ArrowUp, MessageSquare, MessagesSquare, Plus } from "lucide-react"
import { z } from "zod"
import { request } from "@/api"

const chat = z.object({ id: z.string(), title: z.string() })
const chats = z.object({ items: z.array(chat), next: z.string() })
const events = z.object({
  items: z.array(
    z.object({
      id: z.number(),
      kind: z.string(),
      text: z.string(),
      state: z.string(),
      retries: z.number(),
      reply: z.string(),
      reply_id: z.string(),
      action: z.string(),
      action_status: z.string(),
      run: z.string(),
      error: z.string(),
    })
  ),
  next: z.string(),
})
const receipt = z.object({ id: z.number() })
const stateNames: Record<string, string> = {
  pending: "已入队",
  running: "处理中",
  dead: "执行失败",
  done: "已完成",
}
const actionNames: Record<string, string> = {
  pending_approval: "待审批",
  approved: "已批准",
  rejected: "已拒绝",
  executing: "执行中",
  succeeded: "已执行",
  failed: "执行失败",
  unknown: "待核对",
}
function link(id: string) {
  return "#chat?" + new URLSearchParams({ id })
}
function newKey() {
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (v) =>
    v.toString(16).padStart(2, "0")
  ).join("")
}

// Only the opaque, unacknowledged request ID survives a refresh in this tab.
function pendingKey(chat: string, value?: string) {
  try {
    const storageKey = "agentd:chat-send:" + chat
    if (value === undefined) return sessionStorage.getItem(storageKey) || ""
    if (value) sessionStorage.setItem(storageKey, value)
    else sessionStorage.removeItem(storageKey)
  } catch {
    /* The in-memory key still covers retries during this visit. */
  }
  return ""
}

export function OwnerChat({ id, paused }: { id: string; paused: boolean }) {
  const list = useInfiniteQuery({
    queryKey: ["web-chats"],
    initialPageParam: "",
    queryFn: ({ pageParam, signal }) =>
      request("chats?cursor=" + pageParam, chats, { signal }),
    getNextPageParam: (p) => p.next || undefined,
    refetchInterval: 10000,
  })
  const create = useMutation({
    mutationFn: () => request("chats", chat, { method: "POST", body: "{}" }),
    onSuccess: (c) => location.assign(link(c.id)),
  })
  const entries = list.data?.pages.flatMap((p) => p.items) ?? []
  const selected = entries.find((c) => c.id === id)
  return (
    <div className="chat-layout panel">
      <aside className="chat-index" aria-label="网页会话">
        <div className="chat-head">
          <h2>我的会话</h2>
          <button
            className="outline-btn"
            onClick={() => create.mutate()}
            disabled={create.isPending}
          >
            <Plus size={15} />
            新建会话
          </button>
        </div>
        {create.error && <p role="alert">{create.error.message}</p>}
        {list.error && (
          <p role="alert">
            {list.error.message}{" "}
            <button onClick={() => void list.refetch()}>重试</button>
          </p>
        )}
        {list.isPending && <p role="status">正在读取会话…</p>}
        <nav aria-label="对话列表">
          {id && !selected && (
            <a href={link(id)} aria-current="page">
              <MessageSquare size={15} />
              当前会话
            </a>
          )}
          {entries.map((c) => (
            <a
              key={c.id}
              href={link(c.id)}
              aria-current={c.id === id ? "page" : undefined}
            >
              <MessageSquare size={15} />
              <span>{c.title}</span>
            </a>
          ))}
        </nav>
        {!id && !entries.length && list.isSuccess && (
          <p className="muted">尚无已发送的对话</p>
        )}
        {list.hasNextPage && (
          <button
            className="icon-button"
            disabled={list.isFetching}
            onClick={() => void list.fetchNextPage()}
          >
            更多会话
          </button>
        )}
      </aside>
      {id ? (
        <Thread
          key={id}

          id={id}
          title={selected?.title || "新会话"}
          paused={paused}
        />
      ) : (
        <div className="chat-empty">
          <MessagesSquare size={30} />
          <h2>与数字员工对话</h2>
          <p>选择已有会话，或新建会话发送第一条消息。</p>
        </div>
      )}
    </div>
  )
}

function Thread({
  id,
  title,
  paused,
}: {
  id: string
  title: string
  paused: boolean
}) {
  const client = useQueryClient()
  const path = "chats/" + encodeURIComponent(id)
  const feed = useInfiniteQuery({
    queryKey: ["chat-events", id],
    initialPageParam: "",
    queryFn: ({ pageParam, signal }) =>
      request(path + "/messages?cursor=" + pageParam, events, {
        signal,
      }),
    getNextPageParam: (p) => p.next || undefined,
    refetchInterval: 2000,
  })
  const [draft, setDraft] = useState("")
  const [unconfirmed, setUnconfirmed] = useState(() => pendingKey(id))
  const scroll = useRef<HTMLDivElement>(null)
  const follow = useRef(true)
  const older = useRef<{ height: number; top: number } | null>(null)
  const rows = (feed.data?.pages.flatMap((p) => p.items) ?? []).reverse()
  const submitting = useRef(false)
  function acknowledge(key: string) {
    if (pendingKey(id) === key) pendingKey(id, "")
    setUnconfirmed("")
    setDraft("")
    follow.current = true
    void client.invalidateQueries({ queryKey: ["chat-events", id] })
    void client.invalidateQueries({ queryKey: ["web-chats"] })
  }
  const send = useMutation({
    mutationFn: ({ text, key }: { text: string; key: string }) =>
      request(path + "/messages", receipt, {
        method: "POST",
        body: JSON.stringify({ text, key }),
      }),
    onSuccess: (_result, submitted) => acknowledge(submitted.key),
    onSettled: () => {
      submitting.current = false
    },
  })
  const verified = useQuery<z.infer<typeof receipt>>({
    queryKey: ["chat-submission", id, unconfirmed],
    enabled: !!unconfirmed && !send.isPending,
    refetchInterval: (q) => (q.state.data?.id ? false : 2000),
    queryFn: ({ signal }) =>
      request(
        path + "/submissions/" + encodeURIComponent(unconfirmed),
        receipt,
        { signal }
      ),
    staleTime: 0,
  })
  const accepted = !!unconfirmed && !!verified.data?.id
  const oldCount = feed.data?.pages.length ?? 0
  useLayoutEffect(() => {
    const el = scroll.current
    if (!el) return
    if (older.current && !feed.isFetchingNextPage) {
      el.scrollTop = older.current.top + el.scrollHeight - older.current.height
      older.current = null
    } else if (follow.current && !older.current) el.scrollTop = el.scrollHeight
  }, [feed.data, feed.isFetchingNextPage, oldCount])
  function submit() {
    if (accepted && !send.isPending) {
      acknowledge(unconfirmed)
      send.reset()
      return
    }
    if (
      !draft.trim() ||
      submitting.current ||
      send.isPending ||
      (unconfirmed && verified.isPending)
    )
      return
    submitting.current = true
    const key = unconfirmed || newKey()
    pendingKey(id, key)
    setUnconfirmed(key)
    send.mutate({ text: draft.trim(), key })
  }
  return (
    <section className="chat-thread" aria-label="对话内容">
      <header className="chat-head">
        <h2>{title}</h2>
        {paused && <small>服务已暂停，消息将排队</small>}
      </header>
      <div
        className="chat-scroll"
        ref={scroll}
        onScroll={(e) => {
          const el = e.currentTarget
          follow.current = el.scrollHeight - el.clientHeight - el.scrollTop < 40
        }}
      >
        {feed.hasNextPage && (
          <button
            className="chat-more"
            disabled={feed.isFetching}
            onClick={() => {
              const el = scroll.current
              if (el)
                older.current = { height: el.scrollHeight, top: el.scrollTop }
              void feed.fetchNextPage()
            }}
          >
            读取更早消息
          </button>
        )}
        {feed.error && (
          <p role="alert">
            {feed.error.message}{" "}
            <button onClick={() => void feed.refetch()}>重试</button>
          </p>
        )}
        {feed.isPending && <p role="status">正在读取对话…</p>}
        {feed.isSuccess && !rows.length && (
          <p className="chat-placeholder">发送第一条消息开始对话。</p>
        )}
        {rows.map((e) => (
          <article key={e.id} className="chat-turn">
            {e.kind === "message" && (
              <div className="bubble mine">
                <small>我</small>
                <p>{e.text}</p>
              </div>
            )}
            <div className="chat-meta">
              {e.kind === "approval" ? "审批续跑 · " : ""}
              {e.state === "done" && e.action_status === "pending_approval"
                ? "等待决定"
                : stateNames[e.state] || e.state}
              {e.state === "pending" && e.retries > 0 ? " · 等待重试" : ""} · #
              {e.id}
              {e.action && (
                <>
                  {" "}
                  ·{" "}
                  <a
                    className="history-link"
                    href={link(id) + "&action=" + encodeURIComponent(e.action)}
                  >
                    {actionNames[e.action_status] || "查看审批"}
                  </a>
                </>
              )}
            </div>
            {e.reply_id && (
              <div className="bubble">
                <small>数字员工</small>
                <p>{e.reply}</p>
              </div>
            )}
            {e.error && <p className="history-note">{e.error}</p>}
            {e.run && (
              <a
                className="history-link"
                href={"#runs?" + new URLSearchParams({ run: e.run })}
              >
                查看运行依据
              </a>
            )}
          </article>
        ))}
      </div>
      <form
        className="chat-compose"
        onSubmit={(e) => {
          e.preventDefault()
          submit()
        }}
      >
        {send.error && !accepted && <p role="alert">{send.error.message}</p>}
        {unconfirmed && !send.isPending && (
          <p className="muted" role="status">
            {verified.isPending
              ? "正在核对上次发送…"
              : verified.data?.id
                ? "上次发送已入队，请查看会话记录。"
                : "尚未取得发送回执，可重新提交原文。"}
          </p>
        )}
        <textarea
          aria-label="消息内容"
          placeholder="给数字员工发消息…"
          rows={2}
          value={draft}
          disabled={send.isPending || accepted}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (
              e.key === "Enter" &&
              (e.ctrlKey || e.metaKey) &&
              !e.nativeEvent.isComposing
            ) {
              e.preventDefault()
              submit()
            }
          }}
        />
        <div>
          <small>Ctrl / ⌘ + Enter 发送</small>
          <button
            className="primary-btn"
            type="submit"
            disabled={
              (!draft.trim() && !accepted) ||
              send.isPending ||
              (!!unconfirmed && verified.isPending)
            }
          >
            <ArrowUp size={15} />
            {send.isPending ? "发送中" : accepted ? "确认回执" : "发送"}
          </button>
        </div>
      </form>
    </section>
  )
}
