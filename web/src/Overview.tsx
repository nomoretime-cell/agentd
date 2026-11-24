import { lazy, Suspense, useEffect, useRef, useState } from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import {
  ArrowDownToLine,
  ArrowLeft,
  ArrowRight,
  ArrowUpRight,
  BookOpenCheck,
  Boxes,
  CheckCheck,
  ChevronRight,
  ClipboardCheck,
  Inbox,
  LayoutDashboard,
  Radio,
  RefreshCw,
  ChartColumn,
  PanelLeft,
  History,
  MessagesSquare,
} from "lucide-react"
import { DailyActivity } from "./features/overview/DailyActivity"
import { LedgerChart } from "./features/overview/LedgerChart"

import { ThemeMenu } from "./ThemeMenu"
import { Brand } from "./Brand"
import { request, statusSchema } from "./api"
import { ActionDetail } from "./features/actions/ActionDetail"
import {
  categories,
  statusNames,
  workSchema,
  type Item,
  type Kind,
} from "./features/overview/items"

const OwnerChat = lazy(() =>
  import("./features/chat/OwnerChat").then((m) => ({ default: m.OwnerChat }))
)

const Knowledge = lazy(() =>
  import("./features/knowledge/Knowledge").then((m) => ({
    default: m.Knowledge,
  }))
)

const RunHistory = lazy(() =>
  import("./features/runs/RunHistory").then((m) => ({ default: m.RunHistory }))
)

const icons = {
  actions: ClipboardCheck,
  inbox: Inbox,
  outbox: ArrowDownToLine,
  drafts: BookOpenCheck,
}
const kinds = Object.keys(categories) as Kind[]
const formatNumber = (n: number) => n.toLocaleString("zh-CN")

export function Overview() {
  const client = useQueryClient()
  const [hash, setHash] = useState(() => location.hash)
  const runtimeView = hash === "#runtime"
  const runsView = hash === "#runs" || hash.startsWith("#runs?")
  const chatView = hash === "#chat" || hash.startsWith("#chat?")
  const knowledgeView = hash === "#knowledge"
  const pageTitle = knowledgeView
    ? "知识库"
    : chatView
      ? "对话"
      : runsView
        ? "会话与运行"
        : "工作概览"
  const routeParams = new URLSearchParams(
    hash.slice(runsView || chatView ? 6 : 1)
  )
  const actionID = routeParams.get("action")
  const [compact, setCompact] = useState(false)
  const [kind, setKind] = useState<Kind>("actions")
  const [offset, setOffset] = useState(0)
  const [expanded, setExpanded] = useState("")
  const trigger = useRef<HTMLButtonElement | null>(null)
  useEffect(() => {
    const change = () => setHash(location.hash)
    window.addEventListener("hashchange", change)
    return () => window.removeEventListener("hashchange", change)
  }, [])
  const status = useQuery({
    queryKey: ["status"],
    queryFn: ({ signal }) => request("status", statusSchema, { signal }),
    refetchInterval: 5000,
  })
  const work = useQuery({
    queryKey: ["work", kind, offset],
    queryFn: ({ signal }) =>
      request(`workbench?kind=${kind}&offset=${offset}`, workSchema, {
        signal,
      }),
    refetchInterval: 5000,
  })
  const service = status.data
  const channel = service?.[service.im_provider]
  const state = service?.paused
    ? "已暂停"
    : service?.active
      ? "正在执行"
      : "待命中"
  const channelName = service?.im_provider === "wecom" ? "企业微信" : "钉钉"
  const connection = !channel?.enabled
    ? "未启用"
    : channel.connected
      ? "已连接"
      : "未连接"
  const updated = Math.min(status.dataUpdatedAt, work.dataUpdatedAt)
  const selectedCount = work.data?.counts[kind] ?? 0
  function select(value: Kind) {
    setKind(value)
    setOffset(0)
    setExpanded("")
  }
  function inspect(item: Item, button: HTMLButtonElement) {
    if (kind !== "actions") {
      setExpanded(expanded === item.id ? "" : item.id)
      return
    }
    trigger.current = button
    location.assign("#" + new URLSearchParams({ action: item.id }).toString())
  }
  return (
    <div className={compact ? "workspace compact" : "workspace"}>
      <aside className="sidebar">
        <Brand subtitle="所有者工作台" />
        <nav aria-label="工作台导航">
          <span className="nav-label">工作台</span>
          <a
            className={
              runtimeView || runsView || chatView || knowledgeView
                ? undefined
                : "nav-main"
            }
            href="#overview"
            title="工作概览"
          >
            <LayoutDashboard size={17} />
            工作概览
          </a>
          <a
            className={runsView ? "nav-main" : undefined}
            href="#runs"
            title="会话与运行"
          >
            <History size={17} />
            会话与运行
          </a>
          <a
            href="#chat"
            title="对话"
            className={chatView ? "nav-main" : undefined}
          >
            <MessagesSquare size={17} />
            对话
          </a>
          <a
            href="#knowledge"
            title="知识库"
            className={knowledgeView ? "nav-main" : undefined}
          >
            <BookOpenCheck size={17} />
            知识库
          </a>
          <span className="nav-label">运行管理</span>
          <a href="#trend" title="每日活动">
            <ChartColumn size={17} />
            每日活动
          </a>
          <a href="#queue" title="待处理事项">
            <Inbox size={17} />
            待处理事项
          </a>
          <a
            className={runtimeView ? "nav-main" : undefined}
            href="#runtime"
            title="运行信息"
          >
            <Radio size={17} />
            运行信息
          </a>
        </nav>
      </aside>
      <main className="canvas" id="overview">
        <header className="topbar">
          <button
            className="sidebar-trigger icon-button"
            aria-label={compact ? "展开侧栏" : "收起侧栏"}
            aria-expanded={!compact}
            onClick={() => setCompact(!compact)}
          >
            <PanelLeft size={16} />
          </button>
          <span>工作台</span>
          <ChevronRight size={14} />
          {pageTitle}
          <div className="date">
            {new Intl.DateTimeFormat("zh-CN", { dateStyle: "full" }).format(
              new Date()
            )}
          </div>
          <ThemeMenu />
        </header>
        <div className="content">
          <div className="page-head">
            <div>
              <h1>{pageTitle}</h1>
            </div>
            {!runsView && !chatView && !knowledgeView && (
              <div className="head-actions">
                <a className="outline-btn" href="#knowledge">
                  上传知识
                </a>
                {service && (
                  <span>
                    <i className={service.paused ? "dot amber" : "dot"} />
                    {state}
                  </span>
                )}
                <button
                  className="outline-btn"
                  onClick={() => {
                    void client.invalidateQueries({ queryKey: ["activity"] })
                    void status.refetch()
                    void work.refetch()
                  }}
                >
                  <RefreshCw
                    size={14}
                    className={
                      status.isFetching || work.isFetching ? "spin" : ""
                    }
                  />
                  刷新
                </button>
              </div>
            )}
          </div>
          {!runsView &&
            !chatView &&
            !knowledgeView &&
            (status.error || work.error) && (
              <p role="alert">{(status.error || work.error)?.message}</p>
            )}
          <nav className="dashboard-views" aria-label="概览视图">
            <a
              href="#overview"
              aria-current={
                runtimeView || runsView || chatView || knowledgeView
                  ? undefined
                  : "page"
              }
            >
              概览
            </a>
            <a href="#runs" aria-current={runsView ? "page" : undefined}>
              会话与运行
            </a>
            <a href="#chat" aria-current={chatView ? "page" : undefined}>
              对话
            </a>
            <a
              href="#knowledge"
              aria-current={knowledgeView ? "page" : undefined}
            >
              知识库
            </a>
            <a href="#runtime" aria-current={runtimeView ? "page" : undefined}>
              运行信息
            </a>
          </nav>
          {knowledgeView && (
            <Suspense fallback={<p role="status">正在打开知识库…</p>}>
              <Knowledge />
            </Suspense>
          )}
          {chatView && (
            <Suspense fallback={<p role="status">正在打开对话…</p>}>
              <OwnerChat
                id={routeParams.get("id") || ""}
                paused={service?.paused ?? false}
              />
            </Suspense>
          )}
          {runsView && (
            <Suspense fallback={<p role="status">正在打开运行记录…</p>}>
              <RunHistory search={hash.slice(6)} />
            </Suspense>
          )}
          {!runtimeView && !runsView && !chatView && !knowledgeView && (
            <DailyActivity
              pending={
                work.data &&
                Object.values(work.data.counts).reduce((n, v) => n + v, 0)
              }
            >
              <section className="panel queue" id="queue">
                <div className="section-head">
                  <div>
                    <h2>{categories[kind]}</h2>
                    <p className="muted">当前待处理事项</p>
                  </div>
                  <span className="counter">
                    {work.data ? selectedCount : "—"} 项
                  </span>
                </div>
                <div className="queue-choices">
                  {kinds.map((k) => {
                    const Icon = icons[k]
                    return (
                      <button
                        key={k}
                        aria-pressed={kind === k}
                        onClick={() => select(k)}
                      >
                        <Icon size={14} />
                        {categories[k]}{" "}
                        {work.data ? formatNumber(work.data.counts[k]) : "—"}
                      </button>
                    )
                  })}
                </div>
                {work.isPending && (
                  <p className="empty" role="status">
                    正在读取待办…
                  </p>
                )}
                {work.data && !work.data.items.length && (
                  <div className="empty">
                    <span className="empty-icon">
                      <CheckCheck size={23} />
                    </span>
                    <strong>
                      {selectedCount ? "本页已处理完毕" : "当前没有此类待办"}
                    </strong>
                  </div>
                )}
                <ul className="queue-list">
                  {work.data?.items.map((item) => (
                    <li key={item.id}>
                      <button
                        onClick={(e) => inspect(item, e.currentTarget)}
                        className="item"
                      >
                        <div className="item-icon">
                          <Boxes size={17} />
                        </div>
                        <div className="item-body">
                          <strong>
                            {kind === "actions" && item.detail
                              ? item.detail
                              : item.title}
                          </strong>
                          <small>
                            {kind === "actions"
                              ? item.title
                              : item.detail || item.id}
                          </small>
                        </div>
                        <span className="pill">
                          {statusNames[item.status] ?? item.status}
                        </span>
                        <ArrowUpRight size={15} />
                      </button>
                      {expanded === item.id && (
                        <div className="inline-detail">
                          <code>{item.id}</code>
                          <p>{item.detail || "无补充说明"}</p>
                        </div>
                      )}
                    </li>
                  ))}
                </ul>
                {(offset > 0 || selectedCount > 20) && (
                  <div className="pagination">
                    <button
                      aria-label="上一页"
                      disabled={!offset}
                      onClick={() => setOffset(Math.max(0, offset - 20))}
                    >
                      <ArrowLeft size={15} />
                    </button>
                    <span>第 {offset / 20 + 1} 页</span>
                    <button
                      aria-label="下一页"
                      disabled={offset + 20 >= selectedCount}
                      onClick={() => setOffset(offset + 20)}
                    >
                      <ArrowRight size={15} />
                    </button>
                  </div>
                )}
              </section>
            </DailyActivity>
          )}
          <section className="panel runtime" id="runtime" hidden={!runtimeView}>
            <div className="section-head">
              <div>
                <h2>运行信息</h2>
              </div>
              <span className="runtime-icon">
                <Radio size={18} />
              </span>
            </div>
            {service && (
              <dl>
                <div>
                  <dt>数字员工</dt>
                  <dd>
                    <i className={service.paused ? "dot amber" : "dot"} />
                    {state}
                  </dd>
                </div>
                <div>
                  <dt>当前模型</dt>
                  <dd>{service.model || "未配置"}</dd>
                </div>
                <div>
                  <dt>执行器</dt>
                  <dd>{service.executor}</dd>
                </div>
                <div>
                  <dt>消息渠道</dt>
                  <dd>{channel?.enabled ? channelName : "未启用"}</dd>
                </div>
                {channel?.enabled && (
                  <div>
                    <dt>渠道连接</dt>
                    <dd>
                      {connection}
                      {channel.discovery_only ? " · 仅发现" : ""}
                    </dd>
                  </div>
                )}
              </dl>
            )}
          </section>
          {runtimeView && <LedgerChart counts={service?.counts} />}
          {!runsView && !chatView && !knowledgeView && !!updated && (
            <div className="timestamp">
              待办与运行状态更新于{" "}
              {new Date(updated).toLocaleTimeString("zh-CN")}
            </div>
          )}
        </div>
      </main>
      {actionID && (
        <ActionDetail
          key={actionID}
          id={actionID}

          statusNames={statusNames}
          close={() => {
            if (runsView || chatView) {
              routeParams.delete("action")
              location.replace(
                (chatView ? "#chat?" : "#runs?") + routeParams.toString()
              )
              return
            }
            history.replaceState(
              null,
              "",
              location.pathname + location.search + "#queue"
            )
            setHash(location.hash)
            requestAnimationFrame(() =>
              trigger.current?.isConnected
                ? trigger.current.focus()
                : document
                    .querySelector<HTMLButtonElement>(
                      '.queue-choices button[aria-pressed="true"]'
                    )
                    ?.focus()
            )
          }}
        />
      )}
    </div>
  )
}
