import { useLayoutEffect, useRef, useState } from "react"
import {
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query"
import {
  ArrowUp,
  Clock,
  LoaderCircle,
  MessageSquare,
  MessagesSquare,
  Plus,
} from "lucide-react"
import { z } from "zod"
import { requestKey, pendingKey } from "@/lib/request-key"
import { request } from "@/api"
import { ReplyMarkdown } from "./ReplyMarkdown"
import { actionStatus } from "@/lib/status"

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
  pending: "消息已排队",
  running: "数字员工正在处理…",
  dead: "处理失败",
  done: "已完成",
}
function link(id: string) {
  return "#chat?" + new URLSearchParams({ id })
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
      <aside className="chat-index" aria-label="网页对话">
        <div className="chat-head">
          <h2>我的对话</h2>
          <button
            className="outline-btn"
            onClick={() => create.mutate()}
            disabled={create.isPending}
          >
            <Plus size={15} />
            新建对话
          </button>
        </div>
        {create.error && <p role="alert">{create.error.message}</p>}
        {list.error && (
          <p role="alert">
            {list.error.message}{" "}
            <button onClick={() => void list.refetch()}>重试</button>
          </p>
        )}
        {list.isPending && <p role="status">正在读取对话…</p>}
        <nav aria-label="对话列表">
          {id && !selected && (
            <a href={link(id)} aria-current="page">
              <MessageSquare size={15} />
              当前对话
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
          <p className="muted">尚无对话</p>
        )}
        {list.hasNextPage && (
          <button
            className="icon-button"
            disabled={list.isFetching}
            onClick={() => void list.fetchNextPage()}
          >
            更多对话
          </button>
        )}
      </aside>
      {id ? (
        <Thread
          key={id}
          id={id}
          title={selected?.title || "新对话"}
          paused={paused}
        />
      ) : (
        <div className="chat-empty">
          <MessagesSquare size={30} />
          <h2>与数字员工对话</h2>
          <p>选择已有对话，或新建对话发送第一条消息。</p>
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
    // Poll quickly while a message or its approved action is still being
    // handled, so continued replies appear promptly.
    refetchInterval: (q) =>
      q.state.data?.pages.some((p) =>
        p.items.some(
          (e) =>
            e.state === "pending" ||
            e.state === "running" ||
            ["pending_approval", "approved", "executing"].includes(
              e.action_status
            )
        )
      )
        ? 2000
        : 10000,
  })
  const storageKey = "agentd:chat-send:" + id
  const textKey = storageKey + ":text"
  const [unconfirmed, setUnconfirmed] = useState(() => pendingKey(storageKey))
  // An unconfirmed send keeps its text, so a reload can resubmit it as is.
  const [draft, setDraft] = useState(() =>
    unconfirmed ? pendingKey(textKey) : ""
  )
  const sentText = useRef(draft)
  // Mirrors sentText for rendering: a retry resends this text, not edits.
  const [original, setOriginal] = useState(draft)
  const scroll = useRef<HTMLDivElement>(null)
  const follow = useRef(true)
  const older = useRef<{ height: number; top: number } | null>(null)
  const rows = (feed.data?.pages.flatMap((p) => p.items) ?? []).reverse()
  const submitting = useRef(false)
  function acknowledge(key: string) {
    const sent = sentText.current
    if (pendingKey(storageKey) === key) {
      pendingKey(storageKey, "")
      pendingKey(textKey, "")
    }
    sentText.current = ""
    setOriginal("")
    setUnconfirmed("")
    // Keep anything typed since; clear only the text that was sent.
    setDraft((current) => (current.trim() === sent ? "" : current))
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
    // Stop after about a minute; the owner can then resubmit the same text.
    refetchInterval: (q) =>
      q.state.data?.id || q.state.dataUpdateCount >= 30 ? false : 2000,
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
    const key = unconfirmed || requestKey()
    if (!unconfirmed || !sentText.current) sentText.current = draft.trim()
    setOriginal(sentText.current)
    pendingKey(storageKey, key)
    pendingKey(textKey, sentText.current)
    setUnconfirmed(key)
    send.mutate({ text: sentText.current, key })
  }
  return (
    <section className="chat-thread" aria-label="对话内容">
      <header className="chat-head">
        <h2>{title}</h2>
        {paused && <small>服务已暂停，消息将排队</small>}
      </header>
      <div
        className="chat-scroll"
        role="log"
        aria-label="对话记录"
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
            <div className="chat-meta" data-state={e.state}>
              {e.state === "running" && (
                <LoaderCircle className="spin" size={15} aria-hidden="true" />
              )}
              {e.state === "pending" && <Clock size={15} aria-hidden="true" />}
              {e.kind === "approval" ? "审批后继续处理 · " : ""}
              {e.state === "done" && e.action_status === "pending_approval"
                ? "等待审批决定"
                : stateNames[e.state] || e.state}
              {e.state === "pending" && e.retries > 0 ? " · 等待重试" : ""}
              {" · "}
              {e.run ? (
                <a
                  className="history-link"
                  href={"#runs?" + new URLSearchParams({ run: e.run })}
                >
                  处理依据 #{e.id}
                </a>
              ) : (
                "#" + e.id
              )}
              {e.action && (
                <>
                  {" "}
                  ·{" "}
                  <a
                    className="history-link"
                    href={link(id) + "&action=" + encodeURIComponent(e.action)}
                  >
                    {actionStatus[e.action_status] || "查看审批"}
                  </a>
                </>
              )}
            </div>
            {e.reply_id && (
              <div className="bubble">
                <small>数字员工</small>
                <ReplyMarkdown text={e.reply} />
              </div>
            )}
            {e.error && <p className="history-note">{e.error}</p>}
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
              ? "正在核对上次提交结果…"
              : verified.data?.id
                ? "上次提交的消息已接收，请查看对话记录。"
                : "尚未确认提交结果，可重新提交原文。"}
          </p>
        )}
        {unconfirmed &&
          !verified.data?.id &&
          original &&
          draft.trim() !== original && (
            <p className="muted" role="status">
              再次发送仍会提交原文「{original}」；修改后的内容会保留在输入框，
              核对提交结果后可另行发送。
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
            {send.isPending ? "发送中" : accepted ? "核对提交结果" : "发送"}
          </button>
        </div>
      </form>
    </section>
  )
}
