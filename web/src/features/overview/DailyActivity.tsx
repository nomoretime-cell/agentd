import { useRef, useState, type ReactNode } from "react"
import { useInfiniteQuery, useQuery } from "@tanstack/react-query"
import { Dialog } from "radix-ui"
import { Activity, Inbox, MessageSquare, Newspaper, X } from "lucide-react"
import { z } from "zod"
import { serverTime } from "@/lib/server-time"
import { request } from "@/api"

const daySchema = z.object({
  date: z.string(),
  coverage: z.enum(["none", "partial", "full"]),
  replies: z.number(),
  news: z.number(),
  calls: z.number(),
  reported: z.number(),
  tokens: z.number(),
})
type Day = z.infer<typeof daySchema>
const overview = z.object({
  days: z.array(daySchema).min(1),
  timezone: z.string(),
  since: z.string(),
  undated_news: z.number(),
})
const details = z.object({
  next: z.string(),
  items: z.array(
    z.object({
      at: z.number(),
      local_at: z.string().optional(),
      id: z.string(),
      total: z.number().nullable(),
      input: z.number().nullable(),
      output: z.number().nullable(),
      event: z.number(),
      attempt: z.string(),
      run: z.string(),
      label: z.string(),
      body: z.string(),
      status: z.string(),
      key: z.string(),
    })
  ),
})
const metrics = {
  replies: { label: "回复", icon: MessageSquare },
  tokens: { label: "Token 用量", icon: Activity },
  news: { label: "新闻推送", icon: Newspaper },
} as const
type Metric = keyof typeof metrics
const keys = Object.keys(metrics) as Metric[]
function value(day: Day, metric: Metric) {
  if (
    day.coverage === "none" ||
    (metric === "tokens" && day.calls > 0 && !day.reported)
  )
    return null
  return day[metric]
}
function coverage(day: Day, metric: Metric) {
  if (day.coverage === "none") return "未采集"
  if (metric === "tokens" && day.calls > day.reported)
    return day.reported + " / " + day.calls + " 次调用已报告"
  if (day.coverage === "partial") return "从开始采集时计"
  return metric === "news"
    ? "已确认的正式推送"
    : metric === "replies"
      ? "已提交的会话回答"
      : "已报告的调用用量"
}
type Selection = { metric: Metric; date: string }

export function DailyActivity({
  pending,
  children,
}: {
  pending?: number
  children: ReactNode
}) {
  const [range, setRange] = useState(7)
  const [metric, setMetric] = useState<Metric>("replies")
  const [selection, setSelection] = useState<Selection | null>(null)
  const origin = useRef<HTMLButtonElement | null>(null)
  const query = useQuery({
    queryKey: ["activity", range],
    queryFn: ({ signal }) =>
      request("activity?days=" + range, overview, { signal }),
    refetchInterval: 30000,
  })
  const data = query.data,
    today = data?.days.at(-1)
  const peak = Math.max(
    1,
    ...(data?.days.map((day) => value(day, metric) ?? 0) ?? [])
  )
  function inspect(metric: Metric, date: string, button: HTMLButtonElement) {
    origin.current = button
    setMetric(metric)
    setSelection({ metric, date })
  }
  return (
    <>
      {query.error && (
        <p role="alert">
          {query.error.message}{" "}
          <button onClick={() => void query.refetch()}>重试</button>
        </p>
      )}
      <section className="stats" aria-label="数字员工摘要">
        {keys.map((key) => {
          const { label, icon: Icon } = metrics[key]
          return (
            <button
              key={key}
              className="stat"
              disabled={!today}
              onClick={(e) =>
                today && inspect(key, today.date, e.currentTarget)
              }
            >
              <span className="stat-label">
                今日{label}
                <Icon size={16} />
              </span>
              <strong className="stat-value">
                {today
                  ? (value(today, key)?.toLocaleString("zh-CN") ?? "—")
                  : "—"}
              </strong>
              <small>{today ? coverage(today, key) : "正在读取"}</small>
            </button>
          )
        })}
        <a className="stat" href="#queue">
          <span className="stat-label">
            当前待处理
            <Inbox size={16} />
          </span>
          <strong className="stat-value">
            {pending?.toLocaleString("zh-CN") ?? "—"}
          </strong>
          <small>审批、异常与草稿</small>
        </a>
      </section>
      <div className="columns">
        <section id="trend" className="panel activity-panel">
          <div className="section-head">
            <div>
              <h2>每日活动</h2>
              <p className="muted">
                {data
                  ? data.days[0].date +
                    " 至 " +
                    today?.date +
                    " · " +
                    data.timezone
                  : "正在读取活动记录…"}
              </p>
            </div>
            <select
              aria-label="统计范围"
              value={range}
              onChange={(e) => setRange(Number(e.target.value))}
            >
              <option value={7}>近 7 天</option>
              <option value={30}>近 30 天</option>
            </select>
          </div>
          <div className="metric-choices" aria-label="趋势指标">
            {keys.map((k) => (
              <button
                key={k}
                aria-pressed={metric === k}
                onClick={() => setMetric(k)}
              >
                {metrics[k].label}
              </button>
            ))}
          </div>
          <div className="trend-scroll">
            <div className="trend-bars">
              {data?.days.map((day) => {
                const n = value(day, metric)
                return (
                  <button
                    key={day.date}
                    aria-label={
                      day.date +
                      " " +
                      metrics[metric].label +
                      " " +
                      (n ?? "未采集")
                    }
                    title={coverage(day, metric)}
                    onClick={(e) => inspect(metric, day.date, e.currentTarget)}
                  >
                    <span className="trend-track">
                      <strong>{n?.toLocaleString("zh-CN") ?? "—"}</strong>
                      <span
                        className="trend-bar"
                        style={{
                          height: (n === null ? 0 : (n / peak) * 72) + "%",
                        }}
                      />
                    </span>
                    <small>{day.date.slice(5)}</small>
                  </button>
                )
              })}
            </div>
          </div>
          {data && (
            <p className="trend-note">
              {today && (
                <>
                  <a
                    className="history-link"
                    href={
                      "#runs?" +
                      new URLSearchParams({
                        view: "conversations",
                        active: "1",
                        from: today.date,
                        to: today.date,
                      })
                    }
                  >
                    查看今日活跃会话
                  </a>{" "}
                  ·{" "}
                </>
              )}
              采集始于 {serverTime(data.since)}。
              {metric === "news" &&
                data.undated_news > 0 &&
                "另有 " + data.undated_news + " 条历史推送缺少确认日期。"}
            </p>
          )}
        </section>
        {children}
      </div>
      {selection && data && (
        <ActivityDetails
          key={selection.date + selection.metric}

          selection={selection}
          timezone={data.timezone}
          day={data.days.find((d) => d.date === selection.date)}
          close={() => setSelection(null)}
          restore={() => origin.current?.focus({ preventScroll: true })}
        />
      )}
    </>
  )
}

const deliveryNames: Record<string, string> = {
  sent: "投递已确认",
  unknown: "待核对",
  pending: "待投递",
  sending: "投递中",
  failed: "投递失败",
}
function ActivityDetails({
  selection,
  timezone,
  day,
  close,
  restore,
}: {
  selection: Selection
  timezone: string
  day?: Day
  close: () => void
  restore: () => void
}) {
  const { date, metric } = selection
  const q = useInfiniteQuery({
    queryKey: ["activity-detail", metric, date],
    initialPageParam: "",
    queryFn: ({ signal, pageParam }) =>
      request(
        "activity/" +
          metric +
          "?" +
          new URLSearchParams({ date, cursor: pageParam }),
        details,
        { signal }
      ),
    getNextPageParam: (last) => last.next || undefined,
  })
  const items = q.data?.pages.flatMap((p) => p.items) ?? []
  return (
    <Dialog.Root
      open
      onOpenChange={(open) => {
        if (!open) close()
      }}
    >
      <Dialog.Portal>
        <Dialog.Overlay className="scrim" />
        <Dialog.Content
          className="sheet"
          aria-describedby={undefined}
          onCloseAutoFocus={(e) => {
            e.preventDefault()
            restore()
          }}
        >
          <div className="section-head">
            <Dialog.Title>{metrics[metric].label}明细</Dialog.Title>
            <Dialog.Close aria-label="关闭统计明细" className="icon-button">
              <X size={16} />
            </Dialog.Close>
          </div>
          <p className="muted">
            {date} · {timezone}
            {day && " · " + coverage(day, metric)}
          </p>
          {q.isPending && <p role="status">正在读取明细…</p>}
          {q.error && (
            <p role="alert">
              {q.error.message}{" "}
              <button onClick={() => void q.refetch()}>重试</button>
            </p>
          )}
          {q.data && !items.length && (
            <p className="empty">
              {day?.coverage === "none"
                ? "当天尚未开始采集"
                : "当天暂无此类记录"}
            </p>
          )}
          {items.map((it) => (
            <section className="activity-item" key={it.id}>
              <h3>
                {metric === "tokens"
                  ? it.label
                  : metric === "news"
                    ? it.label
                    : it.label === "local"
                      ? "本地会话"
                      : it.label}
              </h3>
              <small>
                {serverTime(it.local_at ?? new Date(it.at).toISOString(), true)}{" "}
                ·{" "}
                {metric === "news"
                  ? it.status === "owner"
                    ? "人工核对"
                    : "渠道回执"
                  : metric === "tokens"
                    ? it.status
                    : deliveryNames[it.status] || "本地提交"}
              </small>
              {metric === "tokens" ? (
                <p>
                  输入 {it.input ?? "未报告"} · 输出 {it.output ?? "未报告"} ·
                  总量 {it.total ?? "未报告"}
                  <br />
                  用途 {it.body}
                </p>
              ) : (
                <pre>{it.body}</pre>
              )}
              <small>{it.key || it.id}</small>
              {metric !== "tokens" && (
                <a
                  className="history-link"
                  href={
                    "#runs?" +
                    new URLSearchParams({
                      message: metric === "news" ? "delivery:" + it.id : it.id,
                    })
                  }
                >
                  查看消息与回执
                </a>
              )}
              {it.attempt && (
                <a
                  className="history-link"
                  href={
                    "#runs?" +
                    new URLSearchParams(
                      it.run
                        ? { run: it.run }
                        : {
                            attempt: it.attempt,
                            from: new Date(Date.parse(date) - 86400000)
                              .toISOString()
                              .slice(0, 10),
                            to: new Date(Date.parse(date) + 86400000)
                              .toISOString()
                              .slice(0, 10),
                          }
                    )
                  }
                >
                  查看事件 #{it.event} 的运行记录
                </a>
              )}
            </section>
          ))}
          {q.hasNextPage && (
            <button
              className="icon-button"
              disabled={q.isFetching}
              onClick={() => void q.fetchNextPage()}
            >
              继续读取
            </button>
          )}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
