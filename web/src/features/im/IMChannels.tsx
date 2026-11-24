import { useState, type FormEvent } from "react"
import { useQuery } from "@tanstack/react-query"
import { z } from "zod"
import { request } from "../../api"
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

export default function IMChannels() {
  const query = useQuery({
    queryKey: ["im-channels"],
    queryFn: ({ signal }) => request("im", settings, { signal }),
    refetchInterval: 10000,
  })
  const [action, setAction] = useState<Action>()
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState("")
  const [error, setError] = useState("")
  const data = query.data
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
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!action || busy) return
    const form = new FormData(event.currentTarget)
    const saved = action.type === "save"
    const fields = saved
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
    setBusy(true)
    setError("")
    try {
      await request(
        "im",
        z.object({ saved: z.literal(true) }),
        {
          method: "POST",
          body: JSON.stringify({
            revision: action.revision,
            operation: action.type,
            provider: action.target,
            ...fields,
          }),
        },
        errorMessage
      )
      setAction(undefined)
      setNotice(
        saved
          ? "渠道配置已保存。"
          : action.type === "remove"
            ? "已删除本地渠道配置。"
            : "已保存下次启动使用的渠道。"
      )
    } catch (e) {
      setError(e instanceof Error ? e.message : "未能保存，请核对最新状态。")
    } finally {
      await query.refetch()
      setBusy(false)
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
        <div>
          <small>下次启动使用</small>
          <strong>{names[data.selected]}</strong>
          {data.restart && <span className="pill">待重启生效</span>}
        </div>
      </section>
      {data.restart && (
        <p role="status">重启 Mac 上的 agentd 后，新配置生效。</p>
      )}
      {!data.writable && (
        <p>当前使用示例或临时配置。请使用私有配置文件启动 agentd 后编辑。</p>
      )}
      {(notice || query.error) && (
        <p role="status">{query.error?.message || notice}</p>
      )}
      {data.channels.map((slot) => (
        <section
          className="panel im-card"
          key={slot.provider}
          aria-label={names[slot.provider]}
        >
          <header>
            <h2>{names[slot.provider]}</h2>
            <span className="pill">
              {slot.configured
                ? slot.discovery
                  ? "仅发现模式"
                  : "已配置"
                : "未配置"}
            </span>
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
              disabled={!data.writable || busy}
              onClick={() => open("save", slot)}
            >
              {slot.configured ? "编辑配置" : "添加配置"}
            </button>
            {slot.configured && data.selected !== slot.provider && (
              <button
                className="outline-btn"
                disabled={!data.writable || busy}
                onClick={() => open("select", slot)}
              >
                下次启动使用
              </button>
            )}
            {data.selected === slot.provider && (
              <button
                className="outline-btn"
                disabled={!data.writable || busy}
                onClick={() => open("select", slot, "none")}
              >
                停用渠道
              </button>
            )}
            {slot.configured && (
              <button
                className="outline-btn"
                disabled={
                  !data.writable ||
                  busy ||
                  data.running === slot.provider ||
                  data.selected === slot.provider
                }
                title="运行中或已选中的渠道须先切换或停用并重启"
                onClick={() => open("remove", slot)}
              >
                删除配置
              </button>
            )}
          </div>
          {action?.slot.provider === slot.provider && (
            <form key={action.type + action.target} onSubmit={submit}>
              <fieldset disabled={busy}>
                <legend>
                  {action.type === "save"
                    ? "配置" + names[slot.provider]
                    : action.type === "remove"
                      ? "删除" + names[slot.provider] + "配置"
                      : "确认更改启动渠道"}
                </legend>
                {action.type === "save" ? (
                  <EditFields slot={action.slot} />
                ) : (
                  <Confirmation action={action} data={data} />
                )}
                {error && <p role="alert">{error}</p>}
                <div className="im-controls">
                  <button
                    className="outline-btn"
                    type="submit"
                    disabled={action.revision !== data.revision}
                  >
                    {busy
                      ? "正在保存…"
                      : action.type === "save"
                        ? "保存配置"
                        : action.type === "remove"
                          ? "确认删除"
                          : "确认更改"}
                  </button>
                  <button
                    className="outline-btn"
                    type="button"
                    onClick={() => {
                      setAction(undefined)
                      setError("")
                    }}
                  >
                    取消
                  </button>
                </div>
                {action.revision !== data.revision && (
                  <p role="alert">配置已变化，请取消后重新打开。</p>
                )}
              </fieldset>
            </form>
          )}
        </section>
      ))}
    </div>
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
          将移除本地身份、密钥和渠道设置。会话、审批和投递记录将保留；如需撤销密钥，请在平台后台操作。
        </p>
        <p>{impactText(action.slot)}</p>
      </>
    )
  const current = data.channels.find((c) => c.provider === data.running)
  return (
    <>
      <p>
        下次启动将使用「{names[action.target]}」。
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
