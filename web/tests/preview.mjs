import { startDemo } from "./demo-server.mjs"

const demo = await startDemo({
  port: Number(process.env.DEMO_PORT || 0),
  publicOrigin: process.env.DEMO_ORIGIN,
})
console.log("免令牌演示地址", demo.url)
process.once("SIGINT", () => demo.stop())
process.once("SIGTERM", () => demo.stop())
