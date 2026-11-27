import { lazy } from "react"
import {
  createRootRoute,
  createRoute,
  createRouter,
} from "@tanstack/react-router"
import { z } from "zod"
import { Workbench } from "./Workbench"
import { Dashboard } from "./features/overview/Dashboard"
import { useServiceStatus } from "./api"
import { afterDecision } from "./features/evolution/data"

const Chat = lazy(() =>
  import("./features/chat/OwnerChat").then((m) => ({ default: m.OwnerChat }))
)
const Knowledge = lazy(() =>
  import("./features/knowledge/Knowledge").then((m) => ({
    default: m.Knowledge,
  }))
)
const Evolution = lazy(() =>
  import("./features/evolution/Evolution").then((m) => ({
    default: m.Evolution,
  }))
)
const History = lazy(() =>
  import("./features/runs/RunHistory").then((m) => ({ default: m.RunHistory }))
)
const IM = lazy(() => import("./features/im/IMChannels"))

const root = createRootRoute({ component: Workbench })
const base = { getParentRoute: () => root }
const action = z.string().optional()
// Search values stay strings: parseSearch and stringifySearch use
// URLSearchParams. Invalid values fall back to the defaults.
const overview = createRoute({
  ...base,
  path: "/",
  validateSearch: z.object({
    action,
    type: z
      .enum(["actions", "inbox", "outbox", "drafts"])
      .optional()
      .catch(undefined),
    offset: z
      .string()
      .regex(/^(0|[1-9]\d*)$/)
      .optional()
      .catch(undefined),
    days: z.enum(["7", "30"]).optional().catch(undefined),
  }),
  component: OverviewPage,
})
function OverviewPage() {
  const { type, offset, days } = overview.useSearch()
  const navigate = overview.useNavigate()
  return (
    <Dashboard
      runtime={false}
      filters={{
        type: type ?? "actions",
        offset: Number(offset ?? 0),
        days: days === "30" ? 30 : 7,
      }}
      onFilter={(next) =>
        void navigate({
          // Defaults are left out of the URL; unchanged filters are kept.
          search: (prev) => ({
            ...prev,
            ...(next.type === undefined
              ? {}
              : { type: next.type === "actions" ? undefined : next.type }),
            ...(next.offset === undefined
              ? {}
              : { offset: next.offset ? String(next.offset) : undefined }),
            ...(next.days === undefined
              ? {}
              : { days: next.days === 30 ? ("30" as const) : undefined }),
          }),
        })
      }
    />
  )
}
const runtime = createRoute({
  ...base,
  path: "runtime",
  component: () => <Dashboard runtime />,
})
const im = createRoute({ ...base, path: "im", component: IM })
const chat = createRoute({
  ...base,
  path: "chat",
  validateSearch: z.object({
    id: z
      .string()
      .regex(/^web-[a-f0-9]{48}$/)
      .optional()
      .catch(undefined),
    action,
  }),
  component: ChatPage,
})
function ChatPage() {
  const { id } = chat.useSearch()
  return (
    <Chat id={id ?? ""} paused={useServiceStatus().data?.paused ?? false} />
  )
}
const knowledge = createRoute({
  ...base,
  path: "knowledge",
  validateSearch: z.object({
    doc: z
      .string()
      .regex(/^[0-9a-f]{16}$/)
      .optional()
      .catch(undefined),
    q: z.string().optional(),
  }),
  component: KnowledgePage,
})
function KnowledgePage() {
  const { doc, q } = knowledge.useSearch()
  const navigate = knowledge.useNavigate()
  return (
    <Knowledge
      selected={doc ?? ""}
      query={q ?? ""}
      onSelect={(doc) =>
        void navigate({
          search: (prev) => ({ ...prev, doc: doc || undefined }),
        })
      }
      onSearch={(q) => void navigate({ search: { q: q || undefined } })}
    />
  )
}
const historyGroups = ["记忆整理", "技能与规则", "文件修订", "需核对"] as const
const evolution = createRoute({
  ...base,
  path: "evolution",
  validateSearch: z.object({
    path: z.string().optional(),
    draft: z.string().optional(),
    // Entry source names come from the server; unknown ones show nothing.
    source: z.string().optional().catch(undefined),
    group: z.enum(historyGroups).optional().catch(undefined),
    days: z.enum(["7", "30"]).optional().catch(undefined),
  }),
  component: EvolutionPage,
})
function EvolutionPage() {
  const { path, draft, source, group, days } = evolution.useSearch()
  const navigate = evolution.useNavigate()
  return (
    <Evolution
      path={path ?? ""}
      draft={draft}
      onDecided={(name, decision) => {
        // The receipt may arrive after the owner has left this page; this
        // navigate is bound to evolution and would pull them back.
        const at = router.state.location
        if (at.pathname !== evolution.fullPath) return
        const next = afterDecision(at.search, name, decision)
        if (next)
          void navigate({
            search: (prev) => ({ ...prev, ...next }),
            replace: true,
          })
      }}
      filters={{
        source: source ?? "",
        group: group ?? "",
        days: days === "30" ? 30 : 7,
      }}
      onFilter={(next) =>
        void navigate({
          search: (prev) => ({
            ...prev,
            ...(next.source === undefined
              ? {}
              : { source: next.source || undefined }),
            ...(next.group === undefined
              ? {}
              : {
                  group: historyGroups.find((g) => g === next.group),
                }),
            ...(next.days === undefined
              ? {}
              : { days: next.days === 30 ? ("30" as const) : undefined }),
          }),
        })
      }
    />
  )
}
const scopes = [
  "channel",
  "role",
  "principal",
  "conversation",
  "status",
  "attempt",
]
const runKeys = [
  ...["run", "message", "action", "q", "from", "to", "flag", "active", "event"],
  ...scopes,
  ...scopes.map((key) => "match_" + key),
]
const runs = createRoute({
  ...base,
  path: "runs",
  // Only the keys the runs pages and the Go history filters read pass
  // through; their values are checked by Go. Paging cursors stay out of URLs.
  validateSearch: z.object({
    view: z
      .enum(["runs", "conversations", "deliveries"])
      .optional()
      .catch(undefined),
    ...Object.fromEntries(
      runKeys.map((key) => [key, z.string().optional().catch(undefined)])
    ),
  }),
  component: RunsPage,
})
function searchParams(values: Record<string, unknown>) {
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(values))
    if (typeof value === "string") params.set(key, value)
  return params
}
function RunsPage() {
  return <History search={searchParams(runs.useSearch())} />
}

export const router = createRouter({
  routeTree: root.addChildren([
    overview,
    runtime,
    im,
    chat,
    knowledge,
    evolution,
    runs,
  ]),
  parseSearch: (search) => Object.fromEntries(new URLSearchParams(search)),
  stringifySearch: (search) => {
    const params = searchParams(search)
    return params.size ? "?" + params : ""
  },
  // Keep existing bookmarks and native anchor links on the single Go entry point.
  rewrite: {
    input: ({ url }) => {
      const hash = url.hash.slice(1)
      const [page] = hash.split("?", 1)
      const anchor = ["overview", "queue", "trend"].includes(page)
      // The overview has no page name, so its search is the whole fragment.
      const overviewSearch = page.includes("=")
      url.pathname = anchor || overviewSearch ? "/" : "/" + page
      url.search = overviewSearch ? hash : hash.slice(page.length + 1)
      url.hash = anchor ? page : ""
      return url
    },
    output: ({ url }) => {
      const destination = url.pathname.slice(1) || url.hash.slice(1)
      const hash = destination ? destination + url.search : url.search.slice(1)
      url.pathname = "/"
      url.search = ""
      url.hash = hash
      return url
    },
  },
  defaultErrorComponent: () => (
    <section className="panel" role="alert">
      <h2>页面加载失败</h2>
      <p>请重新加载工作台以获取最新页面。</p>
      <button className="outline-btn" onClick={() => location.reload()}>
        重新加载工作台
      </button>
    </section>
  ),
  defaultNotFoundComponent: () => (
    <p>
      页面不存在。<a href="#overview">返回工作概览</a>
    </p>
  ),
})
declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router
  }
}
