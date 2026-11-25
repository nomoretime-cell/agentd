import { Fragment, Suspense, useEffect, useState } from "react"
import { Outlet, useLocation } from "@tanstack/react-router"
import {
  LayoutDashboard,
  History,
  MessagesSquare,
  BookOpenCheck,
  Brain,
  Radio,
  Cable,
  ChevronRight,
  PanelLeft,
} from "lucide-react"
import { Brand } from "./Brand"
import { ThemeMenu } from "./ThemeMenu"
import { ActionDetail } from "./features/actions/ActionDetail"
import { actionStatus } from "@/lib/status"

const navigation = [
  {
    page: "overview",
    label: "工作概览",
    icon: LayoutDashboard,
    group: "工作台",
  },
  { page: "runs", label: "会话与运行", icon: History },
  { page: "chat", label: "对话", icon: MessagesSquare },
  { page: "knowledge", label: "知识库", icon: BookOpenCheck },
  { page: "evolution", label: "记忆与技能", icon: Brain },
  { page: "runtime", label: "运行状态", icon: Radio, group: "运行管理" },
  { page: "im", label: "IM 渠道", icon: Cable },
]
export function Workbench() {
  const { pathname, searchStr } = useLocation()
  const current = pathname.slice(1) || "overview"
  const pageTitle =
    navigation.find((n) => n.page === current)?.label ?? "工作台"
  // Each page names itself in the tab title and in browser history.
  useEffect(() => {
    document.title = pageTitle + " · agentd 所有者工作台"
  }, [pageTitle])
  const [compact, setCompact] = useState(false)
  const params = new URLSearchParams(searchStr)
  const actionID = params.get("action")
  return (
    <div className={compact ? "workspace compact" : "workspace"}>
      <aside className="sidebar">
        <Brand subtitle="所有者工作台" />
        <nav aria-label="工作台导航">
          {navigation.map(({ page, label, icon: Icon, group }) => (
            <Fragment key={page}>
              {group && <span className="nav-label">{group}</span>}
              <a
                href={"#" + page}
                title={label}
                aria-current={current === page ? "page" : undefined}
              >
                <Icon size={17} />
                {label}
              </a>
            </Fragment>
          ))}
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
          <nav aria-label="当前位置" className="breadcrumb">
            <span>工作台</span>
            <ChevronRight size={14} aria-hidden />
            <span className="current" aria-current="page">
              {pageTitle}
            </span>
          </nav>
          <div className="date">
            {new Intl.DateTimeFormat("zh-CN", { dateStyle: "full" }).format(
              new Date()
            )}
          </div>
          <ThemeMenu />
        </header>
        <div className="content">
          {current !== "overview" && current !== "runtime" && (
            <div className="page-head">
              <h1>{pageTitle}</h1>
            </div>
          )}
          <Suspense fallback={<p role="status">正在打开{pageTitle}…</p>}>
            <Outlet />
          </Suspense>
        </div>
      </main>
      {actionID && (
        <ActionDetail
          key={actionID}
          id={actionID}
          statusNames={actionStatus}
          // The overview opens actions from its queue; return to its selected type.
          fallbackFocus={() =>
            document.querySelector<HTMLElement>('#queue [aria-pressed="true"]')
          }
          close={() => {
            params.delete("action")
            location.replace(
              current === "overview"
                ? params.size
                  ? "#" + params
                  : "#queue"
                : "#" + current + (params.size ? "?" + params : "")
            )
          }}
        />
      )}
    </div>
  )
}
