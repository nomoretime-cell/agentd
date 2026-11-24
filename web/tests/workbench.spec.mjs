import { createServer, request as proxyRequest } from "node:http"
import { once } from "node:events"
import { test, expect } from "@playwright/test"
import { startDemo } from "./demo-server.mjs"
import { startInstance } from "./instance.mjs"

test("所有者审批、回执与刷新后重新解锁", async ({ page, request }) => {
  const instance = await startInstance()
  try {
    const { url, token } = instance
    const unauth = await request.get(url + "/v1/actions")
    expect(unauth.status()).toBe(401)
    expect((await request.get(url + "/assets/missing.js")).status()).toBe(404)
    const denied = await request.post(url + "/v1/control", {
      headers: {
        Authorization: "Bearer " + token,
        Origin: "https://other.invalid",
      },
      data: { command: "pause" },
    })
    expect(denied.status()).toBe(403)
    await page.goto(url)
    async function unlock() {
      await page.getByLabel("访问令牌").fill(token)
      await page.getByRole("button", { name: "进入工作台" }).click()
    }
    await unlock()
    const first = page.getByRole("button", { name: /create_ticket/ }).first()
    await first.click()
    const dialog = page.getByRole("dialog")
    await expect(dialog.getByText("已冻结的执行参数")).toBeVisible()
    await dialog.getByRole("button", { name: "批准动作" }).click()
    await expect(dialog.getByText("已执行", { exact: true })).toBeVisible({
      timeout: 15000,
    })
    expect(instance.executions()).toBe(1)
    await page.reload()
    await expect(
      page.getByRole("heading", { name: "解锁工作台" })
    ).toBeVisible()
    await unlock()
    await expect(dialog.getByText("已执行", { exact: true })).toBeVisible()
    await page.keyboard.press("Escape")
    await expect(dialog).toHaveCount(0)
    await page.setViewportSize({ width: 375, height: 760 })
    await page.getByRole("button", { name: /异常投递/ }).click()
    await expect(page.getByText("当前没有此类待办")).toBeVisible()
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth
      )
    ).toBe(true)
  } finally {
    await instance.stop()
  }
})

test("转发端口上的可信页面可拒绝动作", async ({ page }) => {
  let upstream
  const forwarder = createServer((incoming, outgoing) => {
    const req = proxyRequest(
      new URL(incoming.url, upstream),
      { method: incoming.method, headers: incoming.headers },
      (res) => {
        outgoing.writeHead(res.statusCode, res.headers)
        res.pipe(outgoing)
      }
    )
    req.on("error", () => outgoing.destroy())
    incoming.pipe(req)
  })
  forwarder.listen(0, "127.0.0.1")
  await once(forwarder, "listening")
  const origin = "http://127.0.0.1:" + forwarder.address().port
  const instance = await startInstance(origin)
  try {
    upstream = instance.url
    await page.goto(origin)
    await page.getByLabel("访问令牌").fill(instance.token)
    await page.getByRole("button", { name: "进入工作台" }).click()
    await page
      .getByRole("button", { name: /create_ticket/ })
      .first()
      .click()
    await page.getByRole("button", { name: "拒绝动作" }).click()
    await expect(
      page.getByRole("dialog").getByText("已拒绝", { exact: true })
    ).toBeVisible()
    expect(instance.executions()).toBe(0)
  } finally {
    forwarder.closeAllConnections()
    await new Promise((done) => forwarder.close(done))
    await instance.stop()
  }
})

test("隔离演示直接进入工作台，刷新可继续查看", async ({ page, request }) => {
  const demo = await startDemo()
  try {
    await page.goto(demo.url)
    await expect(page.getByRole("heading", { name: "工作概览" })).toBeVisible()
    await page
      .getByRole("button", { name: /create_ticket/ })
      .first()
      .click()
    await page
      .getByRole("dialog")
      .getByRole("button", { name: "拒绝动作" })
      .click()
    await expect(
      page.getByRole("dialog").getByText("已拒绝", { exact: true })
    ).toBeVisible()
    await page.reload()
    await expect(
      page.getByRole("dialog").getByText("已拒绝", { exact: true })
    ).toBeVisible()
    await expect(page.getByLabel("访问令牌")).toHaveCount(0)
    expect(
      (
        await request.post(demo.url + "/v1/control", {
          data: { command: "pause" },
        })
      ).status()
    ).toBe(404)
    expect(
      (
        await request.get(demo.url + "/v1/status", {
          headers: { Origin: "https://other.invalid" },
        })
      ).status()
    ).toBe(403)
  } finally {
    await demo.stop()
  }
})
