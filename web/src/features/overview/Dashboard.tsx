import { useState } from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import {
  ArrowDownToLine,
  ArrowLeft,
  ArrowRight,
  ArrowUpRight,
  BookOpenCheck,
  Boxes,
  CheckCheck,
  ClipboardCheck,
  Inbox,
  Radio,
  RefreshCw,
} from "lucide-react"
import { DailyActivity } from "./DailyActivity"
import { LedgerChart } from "./LedgerChart"
import { request, useServiceStatus } from "@/api"
import {
  categories,
  itemStatus,
  workSchema,
  type Item,
  type Kind,
} from "./items"

const icons = {
  actions: ClipboardCheck,
  inbox: Inbox,
  outbox: ArrowDownToLine,
  drafts: BookOpenCheck,
}
const kinds = Object.keys(categories) as Kind[]
const formatNumber = (n: number) => n.toLocaleString("zh-CN")

// Filters that a shared or refreshed link restores live in the URL.
export type Filters = { type: Kind; offset: number; days: 7 | 30 }
const defaults: Filters = { type: "actions", offset: 0, days: 7 }

export function Dashboard({
  runtime,
  filters = defaults,
  onFilter = () => {},
}: {
  runtime: boolean
  filters?: Filters
  onFilter?: (next: Partial<Filters>) => void
}) {
  const client = useQueryClient()
  const { type: kind, offset } = filters
  const setOffset = (offset: number) => onFilter({ offset })
  const [expanded, setExpanded] = useState("")
  const status = useServiceStatus()
  const work = useQuery({
    enabled: !runtime,
    queryKey: ["work", kind, offset],
    queryFn: ({ signal }) =>
      request(`workbench?kind=${kind}&offset=${offset}`, workSchema, {
        signal,
      }),
    refetchInterval: 5000,
  })
  const service = status.data
  const channel =
    service && service.im_provider !== "none"
      ? service[service.im_provider]
      : undefined
  const state = service?.paused
    ? "已暂停"
    : service?.active
      ? "正在执行"
      : "待命"
  const channelName = service?.im_provider === "wecom" ? "企业微信" : "钉钉"
  const connection = !channel?.enabled
    ? "未启用"
    : channel.connected
      ? "已连接"
      : "未连接"
  const updated = runtime
    ? status.dataUpdatedAt
    : Math.min(status.dataUpdatedAt, work.dataUpdatedAt)
  const selectedCount = work.data?.counts[kind] ?? 0
  // The server decides the page size; links with other offsets still land
  // on the page that contains them.
  const pageSize = work.data?.limit ?? 20
  const page = Math.floor(offset / pageSize) + 1
  function select(value: Kind) {
    onFilter({ type: value, offset: 0 })
    setExpanded("")
  }
  function inspect(item: Item) {
    if (kind !== "actions") {
      setExpanded(expanded === item.id ? "" : item.id)
      return
    }
    const link = new URLSearchParams({ action: item.id })
    if (kind !== defaults.type) link.set("type", kind)
    if (offset) link.set("offset", String(offset))
    if (filters.days !== defaults.days) link.set("days", String(filters.days))
    location.assign("#" + link.toString())
  }
  return (
    <>
      <div className="page-head">
        <h1>{runtime ? "运行状态" : "工作概览"}</h1>
        <div className="head-actions">
          <a className="outline-btn" href="#knowledge">
            上传文件
          </a>
          {!runtime && service && (
            <a href="#im" className="outline-btn" aria-label="管理消息渠道">
              {channel?.enabled
                ? channelName + " · " + connection
                : "消息渠道未启用"}
            </a>
          )}
          {service && (
            <span>
              <i className={service.paused ? "dot amber" : "dot"} />
              {state}
            </span>
          )}
          <button
            className="outline-btn"
            onClick={() => {
              if (!runtime)
                void client.invalidateQueries({ queryKey: ["activity"] })
              void status.refetch()
              if (!runtime) void work.refetch()
            }}
          >
            <RefreshCw
              size={14}
              className={status.isFetching || work.isFetching ? "spin" : ""}
            />
            刷新
          </button>
        </div>
      </div>
      {(status.error || (!runtime && work.error)) && (
        <p role="alert">{(status.error || work.error)?.message}</p>
      )}
      {!runtime && (
        <DailyActivity
          range={filters.days}
          setRange={(days) => onFilter({ days })}
          pending={
            work.data &&
            Object.values(work.data.counts).reduce((n, v) => n + v, 0)
          }
        >
          <section className="panel queue" id="queue">
            <div className="section-head">
              <div>
                <h2>{categories[kind]}</h2>
                <p className="muted">当前待办</p>
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
                  <button onClick={() => inspect(item)} className="item">
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
                      {itemStatus(kind, item.status)}
                    </span>
                    <ArrowUpRight size={15} />
                  </button>
                  {expanded === item.id && (
                    <div className="inline-detail">
                      <code>{item.id}</code>
                      <p>{item.detail || "无补充说明"}</p>
                      {kind === "drafts" && (
                        <a href="#evolution">审阅草稿差异</a>
                      )}
                    </div>
                  )}
                </li>
              ))}
            </ul>
            {work.data && offset > 0 && offset >= selectedCount && (
              <p role="status">
                本页事项已处理完。
                <button
                  className="outline-btn"
                  onClick={() =>
                    setOffset(
                      Math.max(0, Math.ceil(selectedCount / pageSize) - 1) *
                        pageSize
                    )
                  }
                >
                  回到最后一页
                </button>
              </p>
            )}
            {(offset > 0 || selectedCount > pageSize) && (
              <div className="pagination">
                <button
                  aria-label="上一页"
                  disabled={!offset}
                  onClick={() => setOffset(Math.max(0, (page - 2) * pageSize))}
                >
                  <ArrowLeft size={15} />
                </button>
                <span>第 {page} 页</span>
                <button
                  aria-label="下一页"
                  disabled={page * pageSize >= selectedCount}
                  onClick={() => setOffset(page * pageSize)}
                >
                  <ArrowRight size={15} />
                </button>
              </div>
            )}
          </section>
        </DailyActivity>
      )}
      {runtime && (
        <section className="panel runtime" id="runtime">
          <div className="section-head">
            <div>
              <h2>服务与渠道</h2>
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
                <dt>Git 提交</dt>
                <dd>
                  {service.git_commit ? (
                    <details>
                      <summary aria-label="查看完整 Git 提交号">
                        <code>{service.git_commit.slice(0, 12)}</code>
                      </summary>
                      <code>{service.git_commit}</code>
                    </details>
                  ) : (
                    "构建未记录"
                  )}
                  {service.git_modified && <span>含未提交更改</span>}
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
                <dd>
                  <a href="#im">
                    {channel?.enabled ? channelName : "未启用"} ↗
                  </a>
                </dd>
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
      )}
      {runtime && <LedgerChart counts={service?.counts} />}
      {!!updated && (
        <div className="timestamp">
          {runtime ? "运行状态" : "待办与运行状态"}更新于{" "}
          {new Date(updated).toLocaleTimeString("zh-CN")}
        </div>
      )}
    </>
  )
}
