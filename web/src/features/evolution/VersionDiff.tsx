import type { Delta } from "./data"
export function VersionDiff({
  before,
  after,
}: Pick<Delta, "before" | "after">) {
  const a = before.split("\n"),
    b = after.split("\n")
  let first = 0,
    last = 0
  while (first < a.length && first < b.length && a[first] === b[first]) first++
  while (
    last < a.length - first &&
    last < b.length - first &&
    a[a.length - 1 - last] === b[b.length - 1 - last]
  )
    last++
  if (before === after) return <p className="muted">内容相同</p>
  return (
    <div className="version-diff" data-testid="version-diff">
      <div>
        <small>原有内容</small>
        <pre>{a.slice(first, a.length - last).join("\n") || "（空）"}</pre>
      </div>
      <div>
        <small>修订内容</small>
        <pre>{b.slice(first, b.length - last).join("\n") || "（空）"}</pre>
      </div>
    </div>
  )
}
