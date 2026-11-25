import { useEffect, useState } from "react"
import { Dialog } from "radix-ui"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { X } from "lucide-react"
import { z } from "zod"
import { request } from "@/api"
import { Button } from "@/components/ui/button"
import { viewerInstant } from "@/lib/server-time"

const actionSchema = z.object({
  id: z.string(),
  tool_name: z.string(),
  tool_args: z.string(),
  resume_hint: z.string(),
  status: z.string(),
  result: z.string(),
  expires_at: z.number(),
})
function formatArguments(value: string) {
  try {
    return JSON.stringify(JSON.parse(value), null, 2)
  } catch {
    return value
  }
}
export function ActionDetail({
  id,
  close,
  statusNames,
  fallbackFocus,
}: {
  id: string
  close: () => void
  statusNames: Record<string, string>
  // Where focus returns when the element that opened the dialog is gone.
  fallbackFocus?: () => HTMLElement | null
}) {
  const [opener] = useState(() => document.activeElement)
  const client = useQueryClient()
  const path = "actions/" + encodeURIComponent(id)
  const query = useQuery({
    queryKey: ["action", id],
    queryFn: ({ signal }) => request(path, actionSchema, { signal }),
    // Rejected, succeeded and failed actions no longer change.
    refetchInterval: (q) =>
      ["rejected", "succeeded", "failed"].includes(q.state.data?.status ?? "")
        ? false
        : 3000,
  })
  const mutation = useMutation({
    mutationFn: (decision: "approve" | "reject") =>
      request(path + "/decision", z.object({ id: z.string() }), {
        method: "POST",
        body: JSON.stringify({ decision }),
      }),
    retry: false,
    onSettled: async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: ["action", id] }),
        client.invalidateQueries({ queryKey: ["work"] }),
      ])
    },
  })
  const action = query.data
  const expired = useExpired(action?.expires_at)
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
          onCloseAutoFocus={(event) => {
            const target =
              opener instanceof HTMLElement &&
              opener !== document.body &&
              opener.isConnected
                ? opener
                : fallbackFocus?.()
            if (target) {
              event.preventDefault()
              target.focus({ preventScroll: true })
            }
          }}
        >
          <div className="section-head">
            <Dialog.Title>动作详情</Dialog.Title>
            <Dialog.Close aria-label="关闭详情" className="icon-button">
              <X size={18} />
            </Dialog.Close>
          </div>
          {query.isPending && <p role="status">正在读取动作…</p>}
          {query.error && <p role="alert">{query.error.message}</p>}
          {action && (
            <>
              <div className="detail-title">
                <span className="pill">
                  {statusNames[action.status] ?? action.status}
                </span>
                <h2>{action.tool_name}</h2>
                <code>{action.id}</code>
              </div>
              {action.resume_hint && (
                <section>
                  <h3>请求说明</h3>
                  <p>{action.resume_hint}</p>
                </section>
              )}
              <section>
                <h3>已冻结的执行参数</h3>
                <pre>{formatArguments(action.tool_args)}</pre>
              </section>
              <section>
                <h3>审批截止</h3>
                <p>{viewerInstant(action.expires_at)}</p>
              </section>
              {action.result && (
                <section>
                  <h3>服务端回执</h3>
                  <pre>{action.result}</pre>
                </section>
              )}
              {action.status === "pending_approval" && expired && (
                <p role="status">已过审批截止时间，服务器将按过期处理。</p>
              )}
              {action.status === "pending_approval" && !expired && (
                <div className="decisions">
                  <Button
                    disabled={mutation.isPending || !!query.error}
                    onClick={() => mutation.mutate("approve")}
                  >
                    批准动作
                  </Button>
                  <Button
                    variant="outline"
                    disabled={mutation.isPending || !!query.error}
                    onClick={() => mutation.mutate("reject")}
                  >
                    拒绝动作
                  </Button>
                </div>
              )}
              {mutation.error && <p role="alert">{mutation.error.message}</p>}
            </>
          )}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}

// useExpired turns true when the deadline passes, even if no refetch
// changes the data, so a dialog left open cannot approve too late.
function useExpired(deadline?: number) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (deadline === undefined || now >= deadline) return
    // Timers above 2^31-1 ms fire at once, so long waits re-arm.
    const timer = setTimeout(
      () => setNow(Date.now()),
      Math.min(deadline - now, 2 ** 31 - 1)
    )
    return () => clearTimeout(timer)
  }, [deadline, now])
  return deadline !== undefined && now >= deadline
}
