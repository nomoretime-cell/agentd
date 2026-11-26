import { useState, type FormEvent } from "react"
import { useQuery } from "@tanstack/react-query"
import { z } from "zod"
import { request } from "@/api"
import "./im.css"

const platform = z.enum(["wecom", "dingtalk"])
const choice = z.enum(["wecom", "dingtalk", "none"])
const slotSchema = z.object({
  provider: platform,
  id: z.string(),
  owner: z.string(),
  has_secret: z.boolean(),
  configured: z.boolean(),
  discovery: z.boolean(),
  allow_all_groups: z.boolean(),
  groups: z.array(z.string()),
  impact: z.object({
    events: z.number(),
    actions: z.number(),
    deliveries: z.number(),
  }),
})
const settings = z.object({
  revision: z.string(),
  selected: choice,
  running: choice,
  connected: z.boolean(),
  running_discovery: z.boolean(),
  restart: z.boolean(),
  writable: z.boolean(),
  channels: z.array(slotSchema),
  attempt: z
    .object({ id: z.string(), provider: choice, error: z.string().optional() })
    .optional(),
})
const names = { wecom: "企业微信", dingtalk: "钉钉", none: "全部停用" }
type Slot = z.infer<typeof slotSchema>
type Settings = z.infer<typeof settings>
type Provider = Slot["provider"]
type Action = {
  type: "save" | "remove" | "select"
  slot: Slot
  target: z.infer<typeof choice>
  revision: string
}

function submitChoices(action: Action, data: Settings) {
  const slot = action.slot
  const current = data.running === slot.provider
  const idle = data.running === "none"
  const name = names[slot.provider]
  if (action.type === "remove")
    return [
      {
        label: current ? "确认停用并删除" : "确认删除",
        apply: current || data.selected === slot.provider,
      },
    ]
  if (action.type === "select")
    return [
      {
        label: action.target === "none" ? "确认停用" : "切换到" + name,
        apply: true,
      },
    ]
  if (current) return [{ label: "保存并应用", apply: true }]
  const enable = {
    label: idle ? "保存并启用" : "保存并切换到" + name,
    apply: true,
  }
  const backup = {
    label: idle && !slot.configured ? "仅保存" : "保存为备用",
    apply: false,
  }
  return idle && !slot.configured ? [enable, backup] : [backup, enable]
}

function impactText(slot: Slot) {
  const { events, actions, deliveries } = slot.impact
  return `待处理事件 ${events} · 待核对动作 ${actions} · 未结投递 ${deliveries}`
}
function errorMessage(body: unknown) {
  const result = z.object({ error: z.string() }).safeParse(body)
  if (!result.success) return
  return result.data.error === "conflict"
    ? "配置已被修改，请取消后按最新配置重新操作。"
    : result.data.error
}

// A request the page sends; retry repeats the failed immediate apply.
type Change = Omit<Action, "type"> & { type: Action["type"] | "retry" }

function slotStatus(slot: Slot, data: Settings) {
  if (data.running === slot.provider)
    return data.connected ? "已连接" : "未连接"
  if (!slot.configured) return "未配置"
  return slot.discovery ? "仅发现模式" : "备用"
}

export default function IMChannels() {
  const query = useQuery({
    queryKey: ["im-channels"],
    queryFn: ({ signal }) => request("im", settings, { signal }),
    refetchInterval: (query) =>
      query.state.data?.attempt && !query.state.data.attempt.error
        ? 1000
        : 5000,
  })
  const [action, setAction] = useState<Action>()
  const [pending, setPending] = useState<{
    provider: Provider
    connecting: boolean
  }>()
  const [notice, setNotice] = useState("")
  const [error, setError] = useState("")
  const data = query.data
  const busy = !!pending || (!!data?.attempt && !data.attempt.error)
  function open(
    type: Action["type"],
    slot: Slot,
    target: Action["target"] = slot.provider
  ) {
    if (!data) return
    setNotice("")
    setError("")
    setAction({ type, slot, target, revision: data.revision })
  }
  function cancel() {
    setAction(undefined)
    setError("")
  }
  async function change(req: Change, apply: boolean, fields = {}) {
    if (busy) return
    setPending({
      provider: req.slot.provider,
      connecting: apply && req.type !== "remove" && req.target !== "none",
    })
    setError("")
    setNotice("")
    try {
      await request(
        "im",
        z.object({ saved: z.literal(true) }),
        {
          method: "POST",
          body: JSON.stringify({
            revision: req.revision,
            operation: req.type,
            provider: req.target,
            apply,
            ...fields,
          }),
        },
        errorMessage
      )
      setAction(undefined)
      setNotice(
        req.type === "remove"
          ? "已删除本地渠道配置。"
          : req.target === "none"
            ? "已停用渠道。"
            : apply
              ? "已应用配置，请查看渠道连接状态。"
              : "已保存为备用。"
      )
    } catch (e) {
      setError(e instanceof Error ? e.message : "未能保存，请核对最新状态。")
    } finally {
      await query.refetch()
      setPending(undefined)
    }
  }
  if (!data)
    return query.error ? (
      <p role="alert">
        {query.error.message}{" "}
        <button className="outline-btn" onClick={() => void query.refetch()}>
          重试
        </button>
      </p>
    ) : (
      <p role="status">正在读取渠道…</p>
    )
  return (
    <div className="im-page">
      <section className="panel im-state" aria-label="渠道运行状态">
        <div>
          <small>当前运行</small>
          <strong>
            {data.running === "none" ? "未启用" : names[data.running]}
          </strong>
          {data.running !== "none" && (
            <span>
              <i className={data.connected ? "dot" : "dot amber"} />
              {data.connected ? "已连接" : "未连接"}
              {data.running_discovery && " · 仅发现模式"}
            </span>
          )}
        </div>
        {data.restart && (
          <p role="status">
            已保存的启动配置与当前运行状态不同。下次启动使用：
            {names[data.selected]}。
          </p>
        )}
      </section>
      {!data.writable && (
        <p>当前使用示例或临时配置。请使用私有配置文件启动 agentd 后编辑。</p>
      )}
      {query.error ? (
        <p role="alert">{query.error.message}</p>
      ) : error && error !== data.attempt?.error ? (
        <p role="alert">{error}</p>
      ) : (
        notice && <p role="status">{notice}</p>
      )}
      {data.channels.map((slot) => (
        <ChannelCard
          key={slot.provider}
          slot={slot}
          data={data}
          busy={busy}
          connecting={pending?.provider === slot.provider && pending.connecting}
          action={action?.slot.provider === slot.provider ? action : undefined}
          submitting={!!pending}
          open={open}
          change={change}
          cancel={cancel}
        />
      ))}
    </div>
  )
}

function ChannelCard({
  slot,
  data,
  busy,
  connecting,
  action,
  submitting,
  open,
  change,
  cancel,
}: {
  slot: Slot
  data: Settings
  busy: boolean
  connecting: boolean
  action?: Action
  submitting: boolean
  open: (type: Action["type"], slot: Slot, target?: Action["target"]) => void
  change: (change: Change, apply: boolean, fields?: object) => Promise<void>
  cancel: () => void
}) {
  const current = data.running === slot.provider
  const idle = data.running === "none"
  const name = names[slot.provider]
  const locked = !data.writable || busy
  const request = (type: Change["type"]): Change => ({
    type,
    target: slot.provider,
    slot,
    revision: data.revision,
  })
  return (
    <section className="panel im-card" aria-label={name}>
      <header>
        <h2>{name}</h2>
        <span className="pill">{slotStatus(slot, data)}</span>
      </header>
      {slot.configured && (
        <dl>
          <div>
            <dt>{slot.provider === "wecom" ? "Bot ID" : "Client ID"}</dt>
            <dd>{slot.id || "未填写"}</dd>
          </div>
          <div>
            <dt>密钥</dt>
            <dd>{slot.has_secret ? "已设置" : "未设置"}</dd>
          </div>
          <div>
            <dt>所有者 ID</dt>
            <dd>{slot.owner || "未填写"}</dd>
          </div>
        </dl>
      )}
      <div className="im-controls">
        <button
          className="outline-btn"
          disabled={locked}
          onClick={() => open("save", slot)}
        >
          {slot.configured ? "编辑配置" : "添加" + name}
        </button>
        {slot.configured && !current && (
          <button
            className="outline-btn"
            disabled={locked}
            onClick={() =>
              // With nothing running there is no work to interrupt.
              idle ? void change(request("select"), true) : open("select", slot)
            }
          >
            {(idle ? "启用" : "切换到") + name}
          </button>
        )}
        {current && (
          <button
            className="outline-btn"
            disabled={locked}
            onClick={() => open("select", slot, "none")}
          >
            停用渠道
          </button>
        )}
        {slot.configured && (
          <button
            className="outline-btn"
            disabled={locked}
            onClick={() => open("remove", slot)}
          >
            {current ? "停用并删除" : "删除配置"}
          </button>
        )}
      </div>
      <AttemptStatus
        name={name}
        connecting={connecting}
        attempt={
          data.attempt?.provider === slot.provider ? data.attempt : undefined
        }
        locked={locked}
        retry={(id) => void change(request("retry"), true, { attempt_id: id })}
      />
      {action && (
        <ChannelForm
          action={action}
          data={data}
          busy={busy}
          submitting={submitting}
          change={change}
          cancel={cancel}
        />
      )}
    </section>
  )
}

function AttemptStatus({
  name,
  connecting,
  attempt,
  locked,
  retry,
}: {
  name: string
  connecting: boolean
  attempt?: Settings["attempt"]
  locked: boolean
  retry: (id: string) => void
}) {
  if (connecting || (attempt && !attempt.error))
    return <p role="status">正在连接{name}…</p>
  if (!attempt?.error) return null
  return (
    <div>
      <p role="alert">{attempt.error}</p>
      <button
        className="outline-btn"
        disabled={locked}
        onClick={() => retry(attempt.id)}
      >
        重试连接
      </button>
    </div>
  )
}

function ChannelForm({
  action,
  data,
  busy,
  submitting,
  change,
  cancel,
}: {
  action: Action
  data: Settings
  busy: boolean
  submitting: boolean
  change: (change: Change, apply: boolean, fields?: object) => Promise<void>
  cancel: () => void
}) {
  const slot = action.slot
  const name = names[slot.provider]
  const current = data.running === slot.provider
  const currentSlot = data.channels.find((c) => c.provider === data.running)
  const stale = action.revision !== data.revision
  const choices = submitChoices(action, data)
  const { events, actions, deliveries } = slot.impact
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (busy) return
    const button = (event.nativeEvent as SubmitEvent).submitter
    const apply =
      button instanceof HTMLButtonElement
        ? button.value === "apply"
        : choices[0].apply
    const form = new FormData(event.currentTarget)
    const fields =
      action.type === "save"
        ? {
            id: form.get("id"),
            owner: form.get("owner"),
            secret: form.get("secret"),
            discovery: form.has("discovery"),
            allow_all_groups: form.has("all"),
            groups: String(form.get("groups") ?? "")
              .split(/\r?\n/)
              .map((x) => x.trim())
              .filter(Boolean),
          }
        : {}
    await change(action, apply, fields)
  }
  return (
    <form key={action.type + action.target} onSubmit={submit}>
      <fieldset disabled={busy}>
        <legend>
          {action.type === "save"
            ? "配置" + name
            : action.type === "remove"
              ? "删除" + name + "配置"
              : "更改运行渠道"}
        </legend>
        {action.type === "save" ? (
          <>
            <EditFields slot={slot} />
            {events + actions + deliveries > 0 && (
              <p>
                {impactText(slot)}。更换身份前可
                <a href="#queue">查看待办</a>或
                <a href="#runs?view=deliveries">核对投递</a>。
              </p>
            )}
            {current ? (
              <p>应用时会重新连接{name}。</p>
            ) : (
              currentSlot && (
                <p>
                  若同时切换，将停止{names[currentSlot.provider]}收发。
                  {impactText(currentSlot)}，未结任务保留在原渠道。
                </p>
              )
            )}
          </>
        ) : (
          <Confirmation action={action} data={data} />
        )}
        <div className="im-controls">
          {choices.map(({ label, apply }) => (
            <button
              key={label}
              className="outline-btn"
              type="submit"
              value={apply ? "apply" : "save"}
              disabled={stale}
            >
              {submitting ? "正在提交…" : label}
            </button>
          ))}
          <button className="outline-btn" type="button" onClick={cancel}>
            取消
          </button>
        </div>
        {stale && <p role="alert">配置已变化，请取消后重新打开。</p>}
      </fieldset>
    </form>
  )
}

function EditFields({ slot }: { slot: Slot }) {
  const [discovery, setDiscovery] = useState(slot.discovery)
  const idLabel: Record<Provider, string> = {
    wecom: "Bot ID",
    dingtalk: "Client ID",
  }
  return (
    <>
      <label>
        {idLabel[slot.provider]}
        <input
          name="id"
          required={!slot.id}
          placeholder={slot.id && "留空保留 " + slot.id}
          autoComplete="off"
        />
      </label>
      <label>
        应用密钥
        <input
          name="secret"
          type="password"
          required={!slot.has_secret}
          placeholder={
            slot.has_secret ? "留空保留现有密钥" : "输入平台签发的密钥"
          }
          autoComplete="new-password"
        />
      </label>
      <label>
        所有者 ID
        <input
          name="owner"
          required={!discovery && !slot.owner}
          placeholder={slot.owner && "留空保留 " + slot.owner}
          autoComplete="off"
        />
      </label>
      <label>
        <input
          name="discovery"
          type="checkbox"
          checked={discovery}
          onChange={(e) => setDiscovery(e.target.checked)}
        />
        仅发现模式（记录身份，不接收任务）
      </label>
      <label>
        <input
          name="all"
          type="checkbox"
          defaultChecked={slot.allow_all_groups}
        />
        允许所有群
      </label>
      <label>
        {slot.provider === "wecom" ? "开放的群 ID" : "开放的会话 ID"}
        （每行一个）
        <textarea
          name="groups"
          defaultValue={slot.groups.join("\n")}
          rows={2}
        />
      </label>
    </>
  )
}
function Confirmation({ action, data }: { action: Action; data: Settings }) {
  if (action.type === "remove")
    return (
      <>
        <p>
          {action.slot.provider === data.running && "将先停止该渠道收发。"}
          将移除本地身份、密钥和渠道设置。会话、审批和投递记录将保留；如需撤销密钥，请在平台后台操作。
        </p>
        <p>{impactText(action.slot)}</p>
      </>
    )
  const current = data.channels.find((c) => c.provider === data.running)
  return (
    <>
      <p>
        {action.target === "none"
          ? "将停用当前渠道。"
          : "将启用" + names[action.target] + "。"}
        {current && "当前渠道的未结任务保留在原渠道。"}
      </p>
      {current && (
        <p>
          {names[current.provider]} · {impactText(current)}
        </p>
      )}
    </>
  )
}
