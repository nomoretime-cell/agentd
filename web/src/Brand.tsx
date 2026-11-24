import { Layers3 } from "lucide-react"
export function Brand({ subtitle }: { subtitle?: string }) {
  return (
    <div className="brand">
      <span className="brand-icon">
        <Layers3 size={20} />
      </span>
      <div className="brand-text">
        <strong>agentd</strong>
        {subtitle && <small>{subtitle}</small>}
      </div>
    </div>
  )
}
