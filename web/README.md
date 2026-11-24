# agentd 所有者工作台

W1 提供令牌解锁、服务和 IM 状态、四类待办、冻结参数审批与回执。动作详情链接可在刷新后重新解锁查看。每 5 秒刷新概览，动作详情每 3 秒核对一次状态。

## 本地预览

使用 Node.js 24 和 package.json 指定的 pnpm。在仓库根目录执行：

```sh
make release
pnpm --dir web demo
```

打开终端打印的预览地址即可体验，刷新也无需输入令牌。预览入口只转发本次临时实例的工作台查询和示例审批。演示在独立临时状态目录启动 Go 服务、本地模拟模型和工单接收器，准备两条可审批动作。按 Ctrl+C 后清理演示实例。对正式实例审批会按冻结参数执行真实操作。

## 构建与接入

`make release` 构建带静态资源的 `bin/agentd-web`。与原二进制使用相同的 `-config` 和 `-root`。一个状态根目录只能被一个运行实例拥有。普通 `make build`、`make test` 和 `make vet` 继续只依赖 Go。

浏览器打开与配置一致的地址（默认 `http://127.0.0.1:8090`）。经 SSH 转发到不同端口或使用 HTTPS 反向代理时，在该实例的配置中将 `web_origin` 设为浏览器地址栏确切的来源，例如 `https://agent.example.com`，重启后生效。代理原样转发 Origin。访问令牌只保存在页面内存中。

开发时在单独的开发实例配置 `web_origin: "http://127.0.0.1:5173"`。启动 Go 服务后运行 `pnpm --dir web dev`，Vite 将 /v1 请求转到 127.0.0.1:8090。用独立状态目录和本地替身验证审批。

## 验证

```sh
pnpm --dir web typecheck
pnpm --dir web lint
make release
pnpm --dir web exec playwright install chromium
pnpm --dir web test:e2e
```

本机已有 Google Chrome 时可用 `PLAYWRIGHT_CHANNEL=chrome pnpm --dir web test:e2e`。
