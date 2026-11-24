import { Dialog } from "radix-ui"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { X } from "lucide-react"
import { z } from "zod"
import { request } from "@/api"
import { Button } from "@/components/ui/button"

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
  token,
  close,
  statusNames,
}: {
  id: string
  token: string
  close: () => void
  statusNames: Record<string, string>
}) {
  const client = useQueryClient()
  const path = "actions/" + encodeURIComponent(id)
  const query = useQuery({
    queryKey: ["action", id],
    queryFn: ({ signal }) => request(token, path, actionSchema, { signal }),
    refetchInterval: 3000,
  })
  const mutation = useMutation({
    mutationFn: (decision: "approve" | "reject") =>
      request(token, path + "/decision", z.object({ id: z.string() }), {
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
  return (
    <Dialog.Root
      open
      onOpenChange={(open) => {
        if (!open) close()
      }}
    >
      <Dialog.Portal>
        <Dialog.Overlay className="scrim" />
        <Dialog.Content className="sheet" aria-describedby={undefined}>
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
                <p>{new Date(action.expires_at).toLocaleString("zh-CN")}</p>
              </section>
              {action.result && (
                <section>
                  <h3>服务端回执</h3>
                  <pre>{action.result}</pre>
                </section>
              )}
              {action.status === "pending_approval" && (
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
