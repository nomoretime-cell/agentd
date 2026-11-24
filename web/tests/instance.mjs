// Start the release binary with only synthetic data and loopback dependencies.
import { createServer } from "node:http"
import { spawn } from "node:child_process"
import { once } from "node:events"
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { resolve, join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"

const repo = resolve(import.meta.dirname, "../..")
async function listen(server) {
  server.listen(0, "127.0.0.1")
  await once(server, "listening")
  return "http://127.0.0.1:" + server.address().port
}
export async function startInstance(webOrigin) {
  const cache = join(repo, ".cache")
  await mkdir(cache, { recursive: true })
  const root = await mkdtemp(join(cache, "web-preview-"))
  const tokenPath = join(root, "data/api-token")
  let executions = 0
  const stub = createServer(async (req, res) => {
    res.setHeader("content-type", "application/json")
    if (req.url === "/action") {
      executions++
      res.end(JSON.stringify({ receipt: "本地测试工单已登记" }))
    } else if (req.url === "/chat/completions") {
      const chunks = []
      for await (const chunk of req) chunks.push(chunk)
      const { messages } = JSON.parse(Buffer.concat(chunks).toString())
      let message = { role: "assistant", content: "已记录处理结果。" }
      if (messages[0]?.content.includes("owner's web conversation")) {
        const users = messages.filter((m) => m.role === "user")
        const current = users
          .at(-1)
          .content.trim()
          .replace(/^Event \d+ \(message\): /, "")
        if (current.includes("Host receipt:"))
          message.content = "工单处理已结束，请查看审批回执。"
        else if (current.includes("创建工单")) {
          message = {
            role: "assistant",
            content: "",
            tool_calls: [
              {
                id: "demo-ticket",
                type: "function",
                function: {
                  name: "bash",
                  arguments: JSON.stringify({
                    action: "create_ticket",
                    arguments: { title: current },
                  }),
                },
              },
            ],
          }
        } else if (current.includes("上一条"))
          message.content =
            "本会话上一条消息是：" + (users.at(-2)?.content ?? "尚无历史消息")
        else message.content = "收到，" + current
      }
      res.end(
        JSON.stringify({
          usage: { prompt_tokens: 24, completion_tokens: 8, total_tokens: 32 },
          choices: [{ message }],
        })
      )
    } else {
      res.statusCode = 404
      res.end("{}")
    }
  })
  const stubURL = await listen(stub)
  const reservation = createServer()
  const url = await listen(reservation)
  await new Promise((done) => reservation.close(done))
  const cfg = join(root, "preview.yaml")
  await writeFile(
    cfg,
    JSON.stringify({
      listen: new URL(url).host,
      web_origin: webOrigin,
      model: { endpoint: stubURL + "/chat/completions", name: "本地演示模型" },
      actions: {
        create_ticket: {
          url: stubURL + "/action",
          method: "POST",
          mode: "ask",
        },
      },
      limits: { approval_seconds: 86400, poll_ms: 100 },
    })
  )
  const child = spawn(
    join(repo, "bin/agentd-web"),
    ["-root", root, "-config", cfg],
    { stdio: "ignore" }
  )
  const ended = once(child, "exit")
  // Handle a launch error even before the readiness loop queries the child.
  ended.catch(() => {})
  async function stop() {
    if (child.pid && child.exitCode === null && child.signalCode === null)
      child.kill("SIGTERM")
    await ended.catch(() => {})
    stub.closeAllConnections()
    await new Promise((done) => stub.close(done))
    await rm(root, { recursive: true, force: true })
  }
  try {
    let ready = false
    for (let i = 0; i < 100; i++) {
      if (!child.pid || child.exitCode !== null)
        throw new Error("预览实例启动失败，请先 make release")
      try {
        ready = (await fetch(url + "/healthz")).ok
      } catch {
        /* listener not ready */
      }
      if (ready) break
      await delay(80)
    }
    if (!ready) throw new Error("预览实例启动超时")
    const token = (await readFile(tokenPath, "utf8")).trim()
    const headers = {
      Authorization: "Bearer " + token,
      "Content-Type": "application/json",
    }
    for (const [key, title] of [
      ["handover", "整理工作交接清单"],
      ["review", "登记资料复核工单"],
    ]) {
      const response = await fetch(url + "/v1/action-requests", {
        method: "POST",
        headers,
        body: JSON.stringify({
          key,
          name: "create_ticket",
          arguments: { title },
          text: title,
        }),
      })
      if (!response.ok) throw new Error("无法建立本地审批示例")
    }
    const message = await fetch(url + "/v1/events", {
      method: "POST",
      headers,
      body: JSON.stringify({
        key: "history-preview",
        text: "请记录本次交接结果。",
      }),
    })
    if (!message.ok) throw new Error("无法建立本地运行示例")
    return { url, tokenPath, token, executions: () => executions, stop }
  } catch (error) {
    await stop()
    throw error
  }
}
