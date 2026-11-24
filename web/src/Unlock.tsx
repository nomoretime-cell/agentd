import { useState } from "react"
import { ArrowRight, KeyRound, LoaderCircle } from "lucide-react"
import { request, statusSchema } from "./api"
import { Button } from "@/components/ui/button"
import { Brand } from "./Brand"
import { ThemeMenu } from "./ThemeMenu"
export function Unlock({ unlock }: { unlock: (token: string) => void }) {
  const [value, setValue] = useState("")
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")
  return (
    <main className="unlock-screen">
      <div className="unlock-brand">
        <Brand />
      </div>
      <div className="unlock-theme">
        <ThemeMenu />
      </div>
      <form
        className="unlock-card"
        onSubmit={async (event) => {
          event.preventDefault()
          if (!value.trim() || busy) return
          setBusy(true)
          setError("")
          try {
            await request(value.trim(), "status", statusSchema)
            unlock(value.trim())
          } catch (e) {
            setError(e instanceof Error ? e.message : "解锁失败")
          } finally {
            setBusy(false)
          }
        }}
      >
        <div className="key-icon">
          <KeyRound size={25} />
        </div>
        <h1>解锁工作台</h1>
        <p>使用 agentd 所有者令牌查看运行状态和处理审批。</p>
        <label htmlFor="access-token">访问令牌</label>
        <input
          id="access-token"
          type="password"
          autoComplete="off"
          spellCheck={false}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          required
        />
        <small>
          令牌位于服务状态目录的 <code>data/api-token</code> 文件。
        </small>
        {error && <p role="alert">{error}</p>}
        <Button type="submit" disabled={busy || !value.trim()}>
          {busy ? <LoaderCircle className="spin" /> : null}进入工作台{" "}
          <ArrowRight size={16} />
        </Button>
      </form>
    </main>
  )
}
