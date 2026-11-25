import { Layers3 } from "lucide-react"

// Product name shown in the UI. The binary, service and storage keys keep
// the name agentd, so renaming the product never touches saved data.
export const productName = "在岗"

export function Brand() {
  return (
    <div className="brand">
      <span className="brand-icon">
        <Layers3 size={20} />
      </span>
      <div className="brand-text">
        <strong>{productName}</strong>
        <small>数字员工工作台</small>
      </div>
    </div>
  )
}
