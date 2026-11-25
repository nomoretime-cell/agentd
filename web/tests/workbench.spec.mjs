import { createServer, request as proxyRequest } from "node:http"
import { once } from "node:events"
import { mkdtemp, rm, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { tmpdir } from "node:os"
import { test, expect } from "@playwright/test"
import { startInstance } from "./instance.mjs"

const webHeaders = { "X-Agentd-Workbench": "1" }

test("所有者审批、回执与刷新后直接读回", async ({ page, request }) => {
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
    const nav = page.getByRole("navigation", { name: "工作台导航" })
    await expect(
      nav.getByRole("link", { name: /每日活动|待处理事项/ })
    ).toHaveCount(0)
    await expect(
      page.getByRole("link", { name: "运行状态", exact: true })
    ).toHaveCount(1)
    await page.getByRole("button", { name: "收起侧栏" }).click()
    const runtime = nav.getByRole("link", { name: "运行状态" })
    await runtime.click()
    await expect(page).toHaveURL(/#runtime$/)
    await expect(runtime).toHaveAttribute("aria-current", "page")
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("运行状态")
    await expect(
      page.getByRole("navigation", { name: "当前位置" })
    ).toContainText("运行状态")
    await expect(page.getByText("当前模型", { exact: true })).toBeVisible()
    await page.reload()
    await expect(page.getByRole("heading", { name: "账本概览" })).toBeVisible()
    await nav.getByRole("link", { name: "工作概览" }).click()
    await expect(page.getByLabel("统计范围")).toBeVisible()
    await page.getByRole("link", { name: /当前待处理/ }).click()
    await expect(page).toHaveURL(/#queue$/)
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
    await expect(page.getByLabel("访问令牌")).toHaveCount(0)
    await expect(dialog.getByText("已执行", { exact: true })).toBeVisible()
    await page.keyboard.press("Escape")
    await expect(dialog).toHaveCount(0)
    await page.setViewportSize({ width: 375, height: 760 })
    await expect(nav.getByRole("link")).toHaveCount(7)
    const runs = nav.getByRole("link", { name: "会话与运行" })
    await runs.click()
    await expect(runs).toHaveAttribute("aria-current", "page")
    await page.getByRole("link", { name: "通知投递", exact: true }).click()
    await expect(runs).toHaveAttribute("aria-current", "page")
    await nav.getByRole("link", { name: "工作概览" }).click()
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

test("保存的主题在首屏前生效，CSP 只放行页面自带的脚本", async ({ page }) => {
  const demo = await startInstance()
  try {
    const blocked = []
    page.on("console", (m) => {
      if (/Content Security Policy/i.test(m.text())) blocked.push(m.text())
    })
    await page.goto(demo.url)
    await page.evaluate(() => localStorage.setItem("agentd-theme", "light"))
    // Without the app bundle only the inline script can apply the theme.
    await page.route("**/assets/*.js", (route) => route.abort())
    await page.reload()
    await expect(page.locator("html")).not.toHaveClass(/dark/)
    expect(blocked).toEqual([])
  } finally {
    await demo.stop()
  }
})

test("单一服务直接进入工作台，刷新可继续查看", async ({ page, request }) => {
  const demo = await startInstance()
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
          headers: { ...webHeaders, Origin: demo.url },
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

test("历史运行可筛选、深链接、刷新并返回原列表", async ({ page, request }) => {
  const demo = await startInstance()
  try {
    const prompt = "请记录本次交接结果"
    await expect
      .poll(async () => {
        const res = await request.get(
          demo.url + "/v1/runs?q=" + encodeURIComponent(prompt),
          { headers: webHeaders }
        )
        return (await res.json()).items?.length ?? 0
      })
      .toBeGreaterThan(0)
    await page.goto(demo.url)
    await page
      .getByRole("navigation", { name: "工作台导航" })
      .getByRole("link", { name: "会话与运行" })
      .click()
    await page.getByLabel("关键词").fill(prompt)
    await page.getByRole("button", { name: "查询", exact: true }).click()
    await page
      .getByRole("link", { name: new RegExp(prompt) })
      .first()
      .click()
    const detail = page.getByRole("dialog", { name: "运行详情" })
    await expect(
      detail.getByText("已记录处理结果。", { exact: true })
    ).toBeVisible()
    await page.reload()
    await expect(detail.getByText(prompt + "。", { exact: true })).toBeVisible()
    await page.keyboard.press("Escape")
    await expect(detail).toHaveCount(0)
    await expect(page.getByLabel("关键词")).toHaveValue(prompt)
    await page.setViewportSize({ width: 375, height: 760 })
    await page.getByLabel("关键词").fill("不存在的演示关键词")
    await page.getByRole("button", { name: "查询", exact: true }).click()
    await expect(page.getByText("所选范围内没有匹配记录")).toBeVisible()
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth
      )
    ).toBe(true)
  } finally {
    await demo.stop()
  }
})

test("首页的按日用量能核对到调用及相应运行", async ({ page, request }) => {
  const demo = await startInstance()
  try {
    await expect
      .poll(async () => {
        const r = await request.get(demo.url + "/v1/activity", {
          headers: webHeaders,
        })
        return (await r.json()).days?.at(-1)?.replies
      })
      .toBe(1)
    await page.goto(demo.url)
    const summary = page.getByRole("region", { name: "数字员工摘要" })
    await expect(
      summary.getByRole("button", { name: /今日回复/ })
    ).toContainText("1")
    await summary.getByRole("button", { name: /今日Token 用量/ }).click()
    const detail = page.getByRole("dialog", { name: "Token 用量明细" })
    await expect(detail.getByText(/输入 24 · 输出 8 · 总量 32/)).toBeVisible()
    await detail.getByRole("link", { name: /的运行记录/ }).click()
    await expect(
      page.getByRole("dialog").getByText(/输入 24 · 输出 8 · 总量 32/)
    ).toBeVisible()
    await page.keyboard.press("Escape")
    await page
      .getByRole("navigation", { name: "工作台导航" })
      .getByRole("link", { name: "工作概览" })
      .click()
    await page.getByLabel("统计范围").selectOption("30")
    await expect(
      page.getByRole("group", { name: "每日趋势" }).getByRole("button")
    ).toHaveCount(30)
    // Filters live in the URL, so a refreshed or shared link restores them.
    await page.getByRole("button", { name: /异常投递/ }).click()
    await expect(page).toHaveURL(/#.*days=30/)
    await expect(page).toHaveURL(/type=outbox/)
    await page.reload()
    await expect(page.getByLabel("统计范围")).toHaveValue("30")
    await expect(
      page.getByRole("button", { name: /异常投递/ })
    ).toHaveAttribute("aria-pressed", "true")
    await page.setViewportSize({ width: 375, height: 760 })
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth
      )
    ).toBe(true)
  } finally {
    await demo.stop()
  }
})

test("会话、历史版本和投递回执可双向追溯", async ({ page, request }) => {
  const demo = await startInstance()
  try {
    await expect
      .poll(
        async () =>
          (
            await (
              await request.get(demo.url + "/v1/activity", {
                headers: webHeaders,
              })
            ).json()
          ).days.at(-1).replies
      )
      .toBe(1)
    await page.goto(demo.url + "/#runs?view=conversations")
    await page.getByRole("link", { name: /本地会话/ }).click()
    await page.getByRole("link", { name: /已记录处理结果/ }).click()
    const run = page.getByRole("dialog", { name: "运行详情" })
    const snapshot = run
      .getByRole("group")
      .filter({ has: page.getByText("文档 · MEMORY.md", { exact: true }) })
    await snapshot.getByText("文档 · MEMORY.md", { exact: true }).click()
    await expect(snapshot.getByText(/来源版本 SHA-256/)).toBeVisible()
    await run.getByRole("link", { name: /查看完整回复/ }).click()
    const msg = page.getByRole("dialog", { name: "消息详情" })
    await expect(msg.getByText(/本地已提交/)).toBeVisible()
    await page.reload()
    await msg.getByRole("link", { name: "打开生成这条消息的运行" }).click()
    await expect(run.getByRole("heading", { name: "批次输入" })).toBeVisible()
    await page.keyboard.press("Escape")
    await page.getByRole("link", { name: "通知投递", exact: true }).click()
    await page
      .getByRole("link", { name: /动作等待审批/ })
      .first()
      .click()
    await expect(
      msg.getByRole("heading", { name: "动作等待审批" })
    ).toBeVisible()
    await msg.getByRole("link", { name: "打开生成这条消息的运行" }).click()
    await expect(run.getByRole("link", { name: /查看动作/ })).toBeVisible()
    await page.setViewportSize({ width: 375, height: 760 })
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth
      )
    ).toBe(true)
  } finally {
    await demo.stop()
  }
})

test("对话跨会话、刷新与审批续跑", async ({ page, request }) => {
  const instance = await startInstance()
  try {
    const { url, token } = instance
    expect((await request.get(url + "/v1/chats")).status()).toBe(401)
    expect(
      (
        await request.post(url + "/v1/chats", {
          headers: {
            Authorization: "Bearer " + token,
            Origin: "https://elsewhere.invalid",
          },
          data: {},
        })
      ).status()
    ).toBe(403)
    await page.goto(url)
    await page.getByRole("link", { name: "对话", exact: true }).click()
    await expect(page).toHaveURL(url + "/#chat")
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("对话")
    async function send(text) {
      await page.getByRole("textbox", { name: "消息内容" }).fill(text)
      await page.getByRole("button", { name: "发送", exact: true }).click()
    }
    const thread = page.getByRole("region", { name: "对话内容" })
    await page.getByRole("button", { name: "新建会话" }).click()
    await send("蓝鲸计划")
    await expect(
      thread.getByText("收到，蓝鲸计划", { exact: true })
    ).toBeVisible({ timeout: 10000 })
    const first = page.url()
    await page.getByRole("button", { name: "新建会话" }).click()
    await expect(page).not.toHaveURL(first)
    await send("金鱼计划")
    await expect(
      thread.getByText("收到，金鱼计划", { exact: true })
    ).toBeVisible()
    const second = page.url()
    await page.goto(first)
    await page.reload()
    await send("上一条消息是什么？")
    await expect(
      thread.getByText("本会话上一条消息是：蓝鲸计划", { exact: true })
    ).toBeVisible({ timeout: 10000 })
    await send("创建工单")
    await thread.getByRole("link", { name: "待审批", exact: true }).click()
    const dialog = page.getByRole("dialog")
    await dialog.getByRole("button", { name: "批准动作" }).click()
    await expect(dialog.getByText("已执行", { exact: true })).toBeVisible({
      timeout: 10000,
    })
    await page.keyboard.press("Escape")
    await expect(page).toHaveURL(first)
    await expect(
      thread.getByText("工单处理已结束，请查看审批回执。", { exact: true })
    ).toBeVisible({ timeout: 10000 })
    expect(instance.executions()).toBe(1)
    await page.goto(second)
    await expect(thread.getByText("金鱼计划", { exact: true })).toBeVisible()
    await expect(thread.getByText(/蓝鲸计划|工单处理已结束/)).toHaveCount(0)
    await page.setViewportSize({ width: 375, height: 760 })
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth
      )
    ).toBe(true)
  } finally {
    await instance.stop()
  }
})

test("发送回执中断后，刷新可恢复且不会重复入队", async ({ page, request }) => {
  const demo = await startInstance()
  try {
    await page.goto(demo.url + "/#chat")
    await page.getByRole("button", { name: "新建会话" }).click()
    await expect(page.getByLabel("消息内容")).toBeVisible()
    const id = new URLSearchParams(new URL(page.url()).hash.slice(6)).get("id")
    await page.route("**/v1/chats/*/messages", async (route) => {
      if (route.request().method() !== "POST") return route.continue()
      const res = await route.fetch()
      expect(res.ok()).toBe(true)
      await route.abort("failed")
    })
    await page.getByLabel("消息内容").fill("只创建一次")
    await page.getByRole("button", { name: "发送", exact: true }).click()
    await expect(page.getByRole("button", { name: "确认回执" })).toBeVisible()
    await page.reload()
    await page.getByRole("button", { name: "确认回执" }).click()
    await expect(
      page.getByText("收到，只创建一次", { exact: true })
    ).toBeVisible({ timeout: 10000 })
    const p = await request.get(demo.url + "/v1/chats/" + id + "/messages", {
      headers: webHeaders,
    })
    expect((await p.json()).items).toHaveLength(1)
  } finally {
    await demo.stop()
  }
})

test("从访问电脑上传，另一个浏览器能检索服务器原件的正文", async ({
  page,
  browser,
}) => {
  const instance = await startInstance()
  const viewer = await browser.newContext()
  const clientDir = await mkdtemp(join(tmpdir(), "agentd-client-"))
  try {
    const source = join(clientDir, "异地交接.md"),
      invalid = join(clientDir, "无效.txt")
    await writeFile(source, "# 跨机资料\n鹭岛交接编号321")
    await writeFile(invalid, Buffer.from([0, 0, 0]))
    await page.goto(instance.url)
    await page.getByRole("link", { name: "上传知识" }).click()
    await page.getByLabel("从这台电脑上传").setInputFiles([source, invalid])
    await page.getByRole("button", { name: "开始上传" }).click()
    await expect(page.getByRole("button", { name: "重试失败项" })).toBeVisible()
    await page.getByRole("button", { name: "重试失败项" }).click()
    await expect(
      page.getByRole("button", { name: "移除", exact: true })
    ).toHaveCount(1)
    await rm(source)
    await page.goto("about:blank")
    const other = await viewer.newPage()
    await other.goto(instance.url + "/#knowledge")
    await other.getByLabel("检索正文").fill("鹭岛交接编号321")
    await other.getByRole("button", { name: "检索", exact: true }).click()
    await other.getByRole("button", { name: /鹭岛交接编号321/ }).click()
    await expect(other.getByTestId("document-body")).toHaveText(
      "# 跨机资料\n鹭岛交接编号321"
    )
    const documentURL = other.url()
    expect(documentURL).toMatch(/doc=[a-f0-9]{16}/)
    await other.reload()
    await expect(other.getByLabel("检索正文")).toHaveValue("鹭岛交接编号321")
    await expect(other.getByTestId("document-body")).toContainText(
      "鹭岛交接编号321"
    )
    await other.getByRole("button", { name: "收起正文" }).click()
    await expect(other.getByTestId("document-body")).toHaveCount(0)
    await other.goBack()
    await expect(other).toHaveURL(documentURL)
    await expect(other.getByTestId("document-body")).toContainText(
      "鹭岛交接编号321"
    )
    await other.goForward()
    await expect(other.getByTestId("document-body")).toHaveCount(0)
    await other.getByText(/2 个文件 · 含失败项/).click()
    await expect(other.getByText("异地交接.md · 已入库")).toBeVisible()
    await other.getByRole("button", { name: "跨机资料", exact: true }).click()
    other.once("dialog", (d) => d.accept())
    await other.getByRole("button", { name: "归档文档" }).click()
    await expect(other.getByText("还没有入库文档。")).toBeVisible()
    await other.setViewportSize({ width: 375, height: 740 })
    expect(
      await other.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth
      )
    ).toBe(true)
  } finally {
    await rm(clientDir, { recursive: true, force: true })
    await viewer.close()
    await instance.stop()
  }
})

// Hold the reply so the browser observes the in-flight state deterministically.
async function whileSubmitting(page, endpoint, button, check) {
  let release
  const gate = new Promise((done) => {
    release = done
  })
  await page.route(
    endpoint,
    async (route) => {
      await gate
      await route.continue()
    },
    { times: 1 }
  )
  const sent = page.waitForRequest(endpoint)
  try {
    await button.click()
    await sent
    await check()
  } finally {
    release()
  }
}

test("记忆可追溯地修订，过期编辑保留，技能审批后生效，整理按所选模型执行", async ({
  page,
  browser,
}) => {
  const instance = await startInstance(undefined, true)
  const context = await browser.newContext()
  try {
    await page.goto(instance.url + "/#evolution?path=MEMORY.md")
    const second = await context.newPage()
    await second.goto(page.url())
    for (const tab of [page, second])
      await tab.getByRole("button", { name: "编辑内容" }).click()
    await second.getByLabel("修订正文").fill("第二份未提交编辑")
    await page.getByLabel("修订正文").fill("工作台人工修订")
    await whileSubmitting(
      page,
      "**/v1/evolution/edit",
      page.getByRole("button", { name: "保存记忆修订" }),
      () => expect(page.getByLabel("修订正文")).toBeDisabled()
    )
    await expect(page.getByTestId("memory-body")).toHaveText("工作台人工修订")
    await expect(second.getByText(/服务器已有新版本/)).toBeVisible({
      timeout: 12000,
    })
    await expect(second.getByLabel("修订正文")).toHaveValue("第二份未提交编辑")
    await page.getByLabel("历史版本").selectOption({ index: 1 })
    await page.getByRole("button", { name: "以此版本起草修订" }).click()
    await page.getByRole("button", { name: "保存记忆修订" }).click()
    await expect(page.getByTestId("memory-body")).not.toHaveText(
      "工作台人工修订"
    )
    await page
      .getByRole("link", {
        name: "skills/memory-consolidation/SKILL.md",
        exact: true,
      })
      .click()
    await page.getByRole("button", { name: "起草技能修订" }).click()
    const body = page.getByLabel("修订正文")
    await body.fill((await body.inputValue()) + "\n经人工复核的修订")
    await page.getByRole("button", { name: "保存待审草稿" }).click()
    await expect(
      page.getByRole("heading", { name: "待审草稿 · memory-consolidation" })
    ).toBeVisible()
    page.once("dialog", (d) => d.accept())
    await page.getByRole("button", { name: "批准发布" }).click()
    await page
      .getByRole("link", {
        name: "skills/memory-consolidation/SKILL.md",
        exact: true,
      })
      .click()
    await expect(page.getByTestId("memory-body")).toContainText(
      "经人工复核的修订"
    )
    await page.getByRole("link", { name: "MEMORY.md", exact: true }).click()
    await page.getByLabel("整理模型").selectOption("openai")
    await whileSubmitting(
      page,
      "**/v1/evolution/dream",
      page.getByRole("button", { name: "立即整理" }),
      () => expect(page.getByLabel("整理模型")).toBeDisabled()
    )
    await expect(page.getByText(/已提交整理任务/)).toBeVisible()
    expect(
      await page.evaluate(() =>
        Object.keys(sessionStorage).filter((k) => k.startsWith("agentd-dream-"))
      )
    ).toEqual([])
    await page
      .getByRole("group")
      .getByText("MEMORY.md", { exact: true })
      .click({ timeout: 12000 })
    await expect(
      page.getByTestId("version-diff").getByText("已核实的交接习惯")
    ).toBeVisible()
    await expect(page.getByTestId("memory-body")).not.toContainText(
      "已核实的交接习惯"
    )
    await expect(page.getByTestId("memory-body")).toContainText(
      "已核实的交接习惯",
      { timeout: 15000 }
    )
    await page.getByLabel("整理模型").selectOption("gemini")
    await page.getByRole("button", { name: "立即整理" }).click()
    await expect(page.getByText("无变化", { exact: true })).toBeVisible({
      timeout: 12000,
    })
    await page.reload()
    await page
      .getByRole("link", { name: "查看运行依据", exact: true })
      .first()
      .click()
    await expect(page.getByRole("dialog", { name: "运行详情" })).toBeVisible()
  } finally {
    await context.close()
    await instance.stop()
  }
})

test("IM 管理可添加、轮换、选择、撤回选择及删除配置", async ({
  page,
  request,
}) => {
  const instance = await startInstance()
  try {
    await page.goto(instance.url)
    await page
      .getByRole("navigation", { name: "工作台导航" })
      .getByRole("link", { name: "IM 渠道" })
      .click()
    const wx = page.getByRole("region", { name: "企业微信" })
    await wx.getByRole("button", { name: "添加配置" }).click()
    await wx.getByLabel("Bot ID").fill("test-robot-1234")
    await wx.getByLabel("应用密钥").fill("test-only-secret")
    await wx.getByLabel("所有者 ID").fill("test-owner-1234")
    await wx.getByRole("button", { name: "保存配置" }).click()
    await expect(wx.getByText("已设置", { exact: true })).toBeVisible()
    await wx.getByRole("button", { name: "编辑配置" }).click()
    await expect(wx.getByLabel("应用密钥")).toHaveValue("")
    await wx.getByLabel("应用密钥").fill("test-only-rotated")
    await wx.getByRole("button", { name: "保存配置" }).click()
    await wx.getByRole("button", { name: "下次启动使用" }).click()
    await wx.getByRole("button", { name: "确认更改" }).click()
    await page.reload()
    await expect(page.getByText("待重启生效", { exact: true })).toBeVisible()
    await expect(wx.getByRole("button", { name: "删除配置" })).toBeDisabled()
    const response = await request.get(instance.url + "/v1/im", {
      headers: webHeaders,
    })
    const body = await response.text()
    expect(body).not.toMatch(/test-only|test-robot|test-owner/)
    expect(JSON.parse(body).running).toBe("none")
    expect(
      (
        await request.post(instance.url + "/v1/im", {
          headers: { ...webHeaders, Origin: "https://other.invalid" },
          data: {},
        })
      ).status()
    ).toBe(403)
    await wx.getByRole("button", { name: "停用渠道" }).click()
    await wx.getByRole("button", { name: "确认更改" }).click()
    await wx.getByRole("button", { name: "删除配置" }).click()
    await wx.getByRole("button", { name: "确认删除" }).click()
    await expect(wx.getByRole("button", { name: "添加配置" })).toBeVisible()
    await page.setViewportSize({ width: 320, height: 700 })
    await expect(page.getByRole("heading", { name: "钉钉" })).toBeVisible()
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth
      )
    ).toBe(true)
  } finally {
    await instance.stop()
  }
})

test("上传收到无效回执后可按原批次核对并重试", async ({ page }) => {
  const instance = await startInstance()
  try {
    const keys = []
    await page.route("**/v1/knowledge/uploads?key=*", async (route) => {
      keys.push(new URL(route.request().url()).searchParams.get("key"))
      if (keys.length === 1)
        return route.fulfill({
          contentType: "text/html",
          body: "<html></html>",
        })
      if (keys.length === 2)
        return route.fulfill({
          json: {
            id: "other",
            created: "now",
            files: [{ name: "批次核对.md", status: "added" }],
          },
        })
      return route.continue()
    })
    await page.goto(instance.url + "/#knowledge")
    await page.getByLabel("从这台电脑上传").setInputFiles({
      name: "批次核对.md",
      mimeType: "text/markdown",
      buffer: Buffer.from("# 批次核对"),
    })
    await page.getByRole("button", { name: "开始上传" }).click()
    const verify = page.getByRole("button", { name: "重新核对回执" })
    const retry = page.getByRole("button", { name: "按原批次重试" })
    await expect(page.getByText("服务器未返回有效的批次回执。")).toBeVisible()
    await verify.click()
    await retry.click()
    await expect.poll(() => keys.length).toBe(2)
    await expect(page.getByText("服务器未返回有效的批次回执。")).toBeVisible()
    await retry.click()
    await expect(
      page.getByRole("button", { name: "批次核对", exact: true })
    ).toBeVisible()
    expect(keys).toHaveLength(3)
    expect(new Set(keys).size).toBe(1)
  } finally {
    await instance.stop()
  }
})

test("服务器使用 Local 时按服务器偏移显示统计和运行时间", async ({ page }) => {
  const instance = await startInstance(undefined, false, "America/New_York")
  try {
    await page.goto(instance.url)
    await expect(page.getByText(/采集始于/)).toContainText(/UTC-0[45]:00/)
    await page.getByRole("link", { name: "会话与运行" }).click()
    await expect(page.getByText(/UTC-0[45]:00/).first()).toBeVisible()
  } finally {
    await instance.stop()
  }
})

test("旧页面缺少按需加载的脚本时保留导航，重新加载可恢复", async ({ page }) => {
  const instance = await startInstance()
  try {
    const background = []
    page.on("request", (r) => {
      if (/^\/v1\/(status|workbench)$/.test(new URL(r.url()).pathname))
        background.push(r.url())
    })
    await page.goto(instance.url + "/#knowledge")
    await expect(page.getByLabel("检索正文")).toBeVisible()
    await page.route("**/assets/IMChannels-*.js", (route) =>
      route.fulfill({ status: 404, body: "missing" })
    )
    await page.getByRole("link", { name: "IM 渠道" }).click()
    await expect(page.getByRole("alert")).toContainText("页面加载失败")
    await expect(
      page.getByRole("navigation", { name: "工作台导航" })
    ).toBeVisible()
    await page.getByRole("link", { name: "知识库", exact: true }).click()
    await expect(page.getByLabel("检索正文")).toBeVisible()
    await page.getByRole("link", { name: "IM 渠道" }).click()
    await page.unroute("**/assets/IMChannels-*.js")
    await page.getByRole("button", { name: "重新加载工作台" }).click()
    await expect(
      page.getByRole("heading", { name: "页面加载失败" })
    ).toHaveCount(0)
    await expect(
      page.getByRole("button", { name: /添加配置|新增配置/ }).first()
    ).toBeVisible()
    expect(background).toEqual([])
  } finally {
    await instance.stop()
  }
})
