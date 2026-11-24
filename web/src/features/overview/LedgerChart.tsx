import type { ServiceStatus } from "@/api"

const labels = {
  inbox: "收件事件",
  timers: "定时计划",
  actions: "动作记录",
  outbox: "通知投递",
} satisfies Record<keyof ServiceStatus["counts"], string>

export function LedgerChart({ counts }: { counts?: ServiceStatus["counts"] }) {
  const keys = Object.keys(labels) as (keyof typeof labels)[]
  const peak = counts ? Math.max(1, ...Object.values(counts)) : 1
  return (
    <section
      className="panel ledger"
      id="ledger"
      aria-labelledby="ledger-title"
    >
      <div className="section-head">
        <div>
          <h2 id="ledger-title">账本概览</h2>
          <p className="muted">当前累计记录，按类别统计</p>
        </div>
      </div>
      {counts ? (
        <dl className="bar-chart">
          {keys.map((key) => (
            <div className="bar-column" key={key}>
              <dt>{labels[key]}</dt>
              <dd>
                <strong>{counts[key].toLocaleString("zh-CN")}</strong>
                <span
                  className="bar"
                  style={{ height: (counts[key] / peak) * 78 + "%" }}
                />
              </dd>
            </div>
          ))}
        </dl>
      ) : (
        <p className="empty">尚无账本数据</p>
      )}
    </section>
  )
}
