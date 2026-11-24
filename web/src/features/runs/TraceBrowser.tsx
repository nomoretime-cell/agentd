import { useInfiniteQuery, useQuery } from "@tanstack/react-query"
import { Dialog } from "radix-ui"
import { X } from "lucide-react"
import { z } from "zod"
import { request } from "@/api"

const message = z.object({
  id: z.string(),
  kind: z.string(),
  target: z.string(),
  body: z.string(),
  status: z.string(),
})
const trace = message.extend({
  confirmed_by: z.string(),
  event: z.number(),
  run: z.string(),
  related_runs: z.array(z.string()).nullable(),
  event_day: z.string(),
  conversation: z.string(),
  key: z.string(),
  at: z.number(),
})
const notices = z.object({ items: z.array(message), next: z.string() })
const scope = z.object({
  role: z.string(),
  principal: z.string(),
  conversation: z.string(),
  count: z.number(),
  active: z.boolean(),
})
const conversations = z.object({
  items: z.array(scope),
  next: z.string(),
  from: z.string(),
  to: z.string(),
  incomplete: z.boolean(),
})
const kinds: Record<string, string> = {
  progress: "渠道已接收任务",
  approval: "动作等待审批",
  failure: "事件达到重试上限",
  "action-result": "动作处理结果",
  reply: "会话回答",
  message: "调用发送消息工具",
  drafts: "草稿待审",
  "memory-held": "记忆整理待审",
  news: "定时新闻推送",
  "news-test": "手动新闻测试",
  notification: "业务通知",
}
const states: Record<string, string> = {
  pending: "待投递",
  sending: "投递中",
  sent: "已确认送达",
  failed: "投递失败",
  unknown: "待核对",
  committed: "本地已提交",
}

function href(params: URLSearchParams, fields: Record<string, string>) {
  const q = new URLSearchParams(params)
  for (const [key, value] of Object.entries(fields)) q.set(key, value)
  return "#runs?" + q
}
function More({
  next,
  busy,
  load,
}: {
  next: boolean
  busy: boolean
  load: () => void
}) {
  return (
    next && (
      <div className="pagination">
        <button disabled={busy} onClick={load}>
          继续读取
        </button>
      </div>
    )
  )
}
export function ConversationList({ filter }: { filter: string }) {
  const q = useInfiniteQuery({
    queryKey: ["conversations", filter],
    initialPageParam: filter,
    queryFn: ({ pageParam, signal }) =>
      request("conversations?" + pageParam, conversations, { signal }),
    getNextPageParam: (last) =>
      last.next
        ? new URLSearchParams([
            ...new URLSearchParams(filter),
            ["from", last.from],
            ["to", last.to],
            ["cursor", last.next],
          ]).toString()
        : undefined,
  })
  const groups = new Map<string, z.infer<typeof scope>>()
  for (const page of q.data?.pages ?? [])
    for (const row of page.items) {
      const key = JSON.stringify([row.role, row.principal, row.conversation])
      const old = groups.get(key)
      groups.set(
        key,
        old
          ? {
              ...old,
              count: old.count + row.count,
              active: old.active || row.active,
            }
          : row
      )
    }
  const day = q.data?.pages[0]
  return (
    <>
      <div className="section-head">
        <div>
          <h2>会话列表</h2>
          <p className="muted">
            {day && day.from + " 至 " + day.to + " · "}按已读取的运行汇总
          </p>
        </div>
      </div>
      {q.isPending && (
        <p className="empty" role="status">
          正在读取会话…
        </p>
      )}
      {q.error && (
        <p role="alert">
          {q.error.message}{" "}
          <button onClick={() => void q.refetch()}>重试</button>
        </p>
      )}
      {q.data?.pages.some((p) => p.incomplete) && (
        <p className="history-note">部分历史记录不完整。</p>
      )}
      {day && !groups.size && <p className="empty">当前范围内尚未找到会话</p>}
      <ul className="history-list">
        {Array.from(groups, ([key, row]) => (
          <li key={key}>
            <a
              href={href(new URLSearchParams(filter), {
                match_role: row.role,
                match_principal: row.principal,
                match_conversation: row.conversation,
              })}
            >
              <span className="history-copy">
                <strong>
                  {row.conversation === "local"
                    ? "本地会话"
                    : row.conversation || "会话未记录"}
                </strong>
                <small>
                  {row.role || "角色未记录"} ·{" "}
                  {row.principal || "服务对象未记录"} ·{" "}
                  {row.active ? "有用户交互" : "后台运行"}
                </small>
              </span>
              <span className="pill">{row.count} 次运行</span>
            </a>
          </li>
        ))}
      </ul>
      <More
        busy={q.isFetching}
        next={q.hasNextPage}
        load={() => void q.fetchNextPage()}
      />
    </>
  )
}
export function DeliveryList() {
  const q = useInfiniteQuery({
    queryKey: ["deliveries"],
    initialPageParam: "",
    queryFn: ({ pageParam, signal }) =>
      request("deliveries?cursor=" + pageParam, notices, { signal }),
    getNextPageParam: (last) => last.next || undefined,
  })
  return (
    <>
      <div className="section-head">
        <h2>通知投递</h2>
        <button
          className="icon-button"
          disabled={q.isFetching}
          onClick={() => void q.refetch()}
        >
          刷新回执
        </button>
      </div>
      {q.isPending && (
        <p className="empty" role="status">
          正在读取投递…
        </p>
      )}
      {q.error && <p role="alert">{q.error.message}</p>}
      {q.data?.pages[0].items.length === 0 && (
        <p className="empty">尚无投递记录</p>
      )}
      <ul className="history-list">
        {q.data?.pages
          .flatMap((p) => p.items)
          .map((m) => (
            <li key={m.id}>
              <a
                href={href(new URLSearchParams("view=deliveries"), {
                  message: m.id,
                })}
              >
                <span className="history-copy">
                  <strong>{m.body}</strong>
                  <small>
                    {kinds[m.kind]} · {m.target}
                  </small>
                </span>
                <span className="pill">{states[m.status] || m.status}</span>
              </a>
            </li>
          ))}
      </ul>
      <More
        busy={q.isFetching}
        next={q.hasNextPage}
        load={() => void q.fetchNextPage()}
      />
    </>
  )
}
export function MessageDetail({
  id,
  params,
}: {
  id: string
  params: URLSearchParams
}) {
  const q = useQuery({
    queryKey: ["message-detail", id],
    queryFn: ({ signal }) =>
      request("replies/" + encodeURIComponent(id), trace, { signal }),
  })
  const m = q.data
  const close = new URLSearchParams(params)
  close.delete("message")
  const destination = new URLSearchParams(close)
  destination.delete("run")
  return (
    <Dialog.Root
      open
      onOpenChange={(open) => {
        if (!open) location.replace("#runs?" + close)
      }}
    >
      <Dialog.Portal>
        <Dialog.Overlay className="scrim" />
        <Dialog.Content className="sheet" aria-describedby={undefined}>
          <div className="section-head">
            <Dialog.Title>消息详情</Dialog.Title>
            <Dialog.Close className="icon-button" aria-label="关闭消息详情">
              <X size={17} />
            </Dialog.Close>
          </div>
          {q.isPending && <p role="status">正在读取消息…</p>}
          {q.error && (
            <p role="alert">
              {q.error.message}{" "}
              <button onClick={() => void q.refetch()}>重试</button>
            </p>
          )}
          {m && (
            <>
              <section>
                <h3>{kinds[m.kind] || m.kind}</h3>
                <pre>{m.body}</pre>
                <small>{m.id}</small>
              </section>
              <section>
                <h3>接收与回执</h3>
                <p>
                  {m.target} · {states[m.status] || m.status}
                </p>
                <p>
                  {m.confirmed_by === "owner"
                    ? "所有者人工核对"
                    : m.confirmed_by === "adapter"
                      ? "投递适配器确认"
                      : ""}{" "}
                  {m.at > 0 && new Date(m.at).toLocaleString("zh-CN")}
                </p>
                <code>{m.key}</code>
              </section>
              <section>
                <h3>消息来源</h3>
                <p>
                  {m.conversation && m.conversation + " · "}
                  {m.event ? "事件 #" + m.event : "来源未知"}
                </p>
                {m.run ? (
                  <a
                    className="history-link"
                    href={href(destination, { run: m.run })}
                  >
                    打开生成这条消息的运行
                  </a>
                ) : m.related_runs?.length ? (
                  m.related_runs.map((r) => (
                    <p key={r}>
                      <a
                        className="history-link"
                        href={href(destination, { run: r })}
                      >
                        查看事件的运行尝试 {r}
                      </a>
                    </p>
                  ))
                ) : m.event > 0 && m.event_day ? (
                  <a
                    className="history-link"
                    href={href(new URLSearchParams(), {
                      event: String(m.event),
                      from: m.event_day,
                      to: m.event_day,
                    })}
                  >
                    在事件日期查找历史运行
                  </a>
                ) : (
                  <p>没有留存运行关联。</p>
                )}
              </section>
            </>
          )}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
