import { useEffect, useRef, useState } from "react"
import { useQuery } from "@tanstack/react-query"
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
  LockKeyhole,
  Radio,
  RefreshCw,
  ChartColumn,
  PanelLeft,
} from "lucide-react"
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

const icons = {
  actions: ClipboardCheck,
  inbox: Inbox,
  outbox: ArrowDownToLine,
  drafts: BookOpenCheck,
}
const kinds = Object.keys(categories) as Kind[]
const readAction = () =>
  new URLSearchParams(location.hash.slice(1)).get("action")
const formatNumber = (n: number) => n.toLocaleString("zh-CN")

export function Overview({
  token,
  lock,
}: {
  token: string
  lock?: () => void
}) {
  const [runtimeView, setRuntimeView] = useState(
    () => location.hash === "#runtime"
  )
  const [compact, setCompact] = useState(false)
  const [kind, setKind] = useState<Kind>("actions")
  const [offset, setOffset] = useState(0)
  const [actionID, setActionID] = useState(readAction)
  const [expanded, setExpanded] = useState("")
  const trigger = useRef<HTMLButtonElement | null>(null)
  useEffect(() => {
    const change = () => {
      setActionID(readAction())
      setRuntimeView(location.hash === "#runtime")
    }
    window.addEventListener("hashchange", change)
    return () => window.removeEventListener("hashchange", change)
  }, [])
  const status = useQuery({
    queryKey: ["status"],
    queryFn: ({ signal }) => request(token, "status", statusSchema, { signal }),
    refetchInterval: 5000,
  })
  const work = useQuery({
    queryKey: ["work", kind, offset],
    queryFn: ({ signal }) =>
      request(token, `workbench?kind=${kind}&offset=${offset}`, workSchema, {
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
        <Brand subtitle={lock ? "所有者工作台" : "演示工作台"} />
        <nav aria-label="工作台导航">
          <span className="nav-label">工作台</span>
          <a
            className={runtimeView ? undefined : "nav-main"}
            href="#overview"
            title="工作概览"
          >
            <LayoutDashboard size={17} />
            工作概览
          </a>
          <span className="nav-label">运行管理</span>
          <a href="#ledger" title="账本概览">
            <ChartColumn size={17} />
            账本概览
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
        {lock && (
          <button className="lock" onClick={lock}>
            <LockKeyhole size={16} />
            锁定工作台
          </button>
        )}
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
          工作概览
          <div className="date">
            {new Intl.DateTimeFormat("zh-CN", { dateStyle: "full" }).format(
              new Date()
            )}
          </div>
          <ThemeMenu />
          {lock && (
            <button
              onClick={lock}
              className="mobile-lock"
              aria-label="锁定工作台"
            >
              <LockKeyhole size={17} />
            </button>
          )}
        </header>
        <div className="content">
          <div className="page-head">
            <div>
              <h1>工作概览</h1>
            </div>
            <button
              className="outline-btn"
              onClick={() => {
                void status.refetch()
                void work.refetch()
              }}
            >
              <RefreshCw
                size={14}
                className={status.isFetching || work.isFetching ? "spin" : ""}
              />
              刷新
            </button>
          </div>
          {(status.error || work.error) && (
            <p role="alert">{(status.error || work.error)?.message}</p>
          )}
          <nav className="dashboard-views" aria-label="概览视图">
            <a href="#overview" aria-current={runtimeView ? undefined : "page"}>
              概览
            </a>
            <a href="#runtime" aria-current={runtimeView ? "page" : undefined}>
              运行信息
            </a>
          </nav>
          <div hidden={runtimeView}>
            <section className="stats" aria-label="待处理事项统计">
              {kinds.map((k) => {
                const Icon = icons[k]
                return (
                  <button
                    key={k}
                    className={"stat " + (kind === k ? "selected" : "")}
                    aria-pressed={kind === k}
                    onClick={() => select(k)}
                  >
                    <span className="stat-label">
                      {categories[k]}
                      <Icon size={17} />
                    </span>
                    <span className="stat-value">
                      {work.data ? formatNumber(work.data.counts[k]) : "—"}
                    </span>
                  </button>
                )
              })}
            </section>
            <div className="columns">
              <LedgerChart counts={service?.counts} />
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
            </div>
          </div>
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
          {!!updated && (
            <div className="timestamp">
              数据更新于 {new Date(updated).toLocaleTimeString("zh-CN")}
            </div>
          )}
        </div>
      </main>
      {actionID && (
        <ActionDetail
          key={actionID}
          id={actionID}
          token={token}
          statusNames={statusNames}
          close={() => {
            history.replaceState(
              null,
              "",
              location.pathname + location.search + "#queue"
            )
            setActionID(null)
            requestAnimationFrame(() =>
              trigger.current?.isConnected
                ? trigger.current.focus()
                : document
                    .querySelector<HTMLButtonElement>(".stat.selected")
                    ?.focus()
            )
          }}
        />
      )}
    </div>
  )
}
