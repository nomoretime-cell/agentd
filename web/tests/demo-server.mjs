// Only the disposable fixture owns this gateway. Production has no anonymous API.
import { once } from "node:events"
import { createServer } from "node:http"
import { startInstance } from "./instance.mjs"

export async function startDemo({ port = 0, publicOrigin } = {}) {
  const instance = await startInstance()
  const origins = new Set()
  const server = createServer(async (req, res) => {
    const origin = "http://" + req.headers.host
    const sendError = (code) => {
      res.writeHead(code)
      res.end()
    }
    if (
      !origins.has(origin) ||
      (req.headers.origin && req.headers.origin !== origin)
    )
      return sendError(403)
    try {
      const url = new URL(req.url, origin)
      if (url.origin !== origin) return sendError(400)
      const path = url.pathname
      const page = req.method === "GET" && path === "/"
      const asset = req.method === "GET" && /^\/assets\/[\w.-]+$/.test(path)
      const read =
        req.method === "GET" &&
        (/^\/v1\/(status|workbench|runs|activity|conversations|deliveries)$/.test(
          path
        ) ||
          /^\/v1\/activity\/(replies|tokens|news)$/.test(path) ||
          /^\/v1\/runs\/(?:\d{4}-\d{2}-\d{2}\.\d+|\d+~[a-f0-9]{48})$/.test(
            path
          ) ||
          /^\/v1\/replies\/(?:delivery|local|reply|message)(?::|%3A)[a-z0-9:%]+$/i.test(
            path
          ) ||
          /^\/v1\/actions\/[a-f0-9]+$/.test(path))
      const decide =
        req.method === "POST" &&
        /^\/v1\/actions\/[a-f0-9]+\/decision$/.test(path)
      if (!page && !asset && !read && !decide) return sendError(404)
      const headers = {}
      let body
      if (read || decide) headers.Authorization = "Bearer " + instance.token
      if (decide) {
        if (
          req.headers.origin !== origin ||
          req.headers["content-type"] !== "application/json"
        )
          return sendError(403)
        body = ""
        for await (const chunk of req) {
          if (body.length + chunk.length > 128) return sendError(413)
          body += chunk.toString()
        }
        headers["Content-Type"] = "application/json"
      }
      const upstream = await fetch(instance.url + path + url.search, {
        method: req.method,
        headers,
        body,
        redirect: "error",
      })
      res.statusCode = upstream.status
      for (const key of [
        "content-type",
        "content-security-policy",
        "x-content-type-options",
        "referrer-policy",
      ]) {
        if (upstream.headers.has(key))
          res.setHeader(key, upstream.headers.get(key))
      }
      res.setHeader("Cache-Control", "no-store")
      res.end(
        page
          ? (await upstream.text()).replace(
              "</head>",
              '<meta name="agentd-isolated-demo"></head>'
            )
          : Buffer.from(await upstream.arrayBuffer())
      )
    } catch {
      sendError(502)
    }
  })
  try {
    server.listen(port, "127.0.0.1")
    await once(server, "listening")
    const url = "http://127.0.0.1:" + server.address().port
    origins.add(url)
    origins.add(url.replace("127.0.0.1", "localhost"))
    if (publicOrigin) origins.add(new URL(publicOrigin).origin)
    return {
      url,
      stop: async () => {
        server.closeAllConnections()
        await new Promise((done) => server.close(done))
        await instance.stop()
      },
    }
  } catch (error) {
    server.close()
    await instance.stop()
    throw error
  }
}
