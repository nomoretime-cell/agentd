# 「在岗」工作台临时公网分享：方案与部署记录

2026-11-25，在运行 agentd 的 Mac 上，为持有共享账号的访客开通了临时 HTTPS 入口。访客只需浏览器。此次使用已有发布版本 `22636a8`，通过 Homebrew 安装 Caddy 2.11.9 和 cloudflared 2026.11.1。应用继续使用原有数据库、工作区和模型配置。

## 访客如何登录（Caddy Basic Auth）

1. **在部署 Mac 上取链接和密码。** 本次公网 HTTPS 链接保存在 `/Users/yejibing/.config/agentd-share/workbench-ujwglrbv/url.txt`，共享账号和密码保存在同目录的 `login.txt`。可在 Mac 上用文本编辑器打开凭据文件：

   ```sh
   open -a TextEdit "/Users/yejibing/.config/agentd-share/workbench-ujwglrbv/login.txt"
   ```

2. **将链接和密码分开发给受邀者。** 访客在手机或电脑浏览器中打开 HTTPS 链接。首次访问时，浏览器会弹出用户名和密码对话框。
3. **在浏览器弹窗中输入用户名 `owner` 和 `login.txt` 中的密码。** Caddy 验证通过后，浏览器会直接显示「在岗」工作台。如果要复核首次登录的效果，可在新的无痕窗口打开同一链接。

## 一次请求怎样到达工作台

```text
访客浏览器
  │ HTTPS，请求临时域名
  ▼
Cloudflare 边缘节点（终止公网 TLS）
  │ 复用 Mac 主动建立的加密 HTTP/2 隧道
  ▼
Mac 上的 cloudflared
  │ HTTP，经本机回环地址
  ▼
Caddy  127.0.0.1:18091（整站 Basic Auth）
  │ 验证成功后反向代理
  ▼
agentd 127.0.0.1:8090（页面、静态资源和工作台 API）
```

Mac 接入局域网，部署时没有用户自有域名，也没有确认路由器具备公网 IPv4 入站条件。Quick Tunnel 分配临时域名，Mac 主动向 Cloudflare 建立连接，路由器无需新增端口映射。原计划使用的本地端口 `8080` 已被占用，因此给 Caddy 选用空闲的 `18091`。两个 HTTP 监听端口均只绑定回环地址。[Quick Tunnel 官方说明](https://developers.cloudflare.com/tunnel/get-started/quick-tunnels/)

**评审时要确认两个信任边界。** 公网 HTTPS 在 Cloudflare 终止，登录请求、对话和上传内容都会经过该服务商。Caddy 将所有访客验证为同一个 `owner`，持有密码的人可以读取现有会话、处理审批和管理 IM 渠道。应用账本可用于追溯操作，但共享账号无法标识具体访客。此入口适用于愿意共同管理同一实例的人。[工作台访问约定](../../web/README.md)

## 实际部署顺序

1. **检查运行实例。** 从本机访问 `http://127.0.0.1:8090/healthz` 得到正常响应，确认监听进程是已有 agentd。在本机用户配置目录创建权限为 `0700` 的分享目录。生成随机密码，以 `0600` 保存本机登录信息；Caddy 配置仅使用 bcrypt 密码哈希。
2. **先验证登录保护。** 启动 Caddy，匿名请求本地代理的首页、状态 API 和健康端点，三者均须返回 `401`。使用正确凭据后，首页和健康检查成功。认证覆盖所有路径，包括 `/v1/*`。
3. **建立隧道并验证公网入口。** 按下面的命令启动 cloudflared，将分配的 HTTPS 地址保存到分享目录的 `url.txt`。经公网重复匿名和带凭据的检查，通过后才切换应用配置。
4. **切换可信来源。** 将 `config.local.yaml` 中的 `web_origin` 从原内网入口改为新 HTTPS 来源，保留 `listen: "127.0.0.1:8090"`。切换脚本检查旧进程和数据库路径，确认 IM 无待重启配置，等到执行器空闲且没有知识导入，再保存原来源、原子替换配置、重启同一个二进制。启动后核对监听进程与原暂停状态，启动异常时尝试恢复原配置及服务。[工作台来源校验](../../internal/agent/http.go)
5. **通过公网验收。** 检查首页、健康端点和状态 API 的响应，再用 Chrome 打开公网域名，确认「在岗」和「数字员工工作台」可见，页面自身发起的状态查询成功。

**Caddy 生效配置的脱敏副本**（密码哈希以占位符表示）

```caddyfile
{
    admin off
    auto_https off
}
http://:18091 {
    bind 127.0.0.1
    basic_auth {
        owner <bcrypt-hash>
    }
    reverse_proxy 127.0.0.1:8090 {
        header_up -Authorization
        header_up Host 127.0.0.1:8090
    }
}
```

Caddy 在本机验证密码后移除上游请求的 `Authorization`，把 `Host` 设为 agentd 的监听地址，原样转发浏览器的 `Origin`。前端在工作台 API 请求中附加 `X-Agentd-Workbench: 1`。Go 对工作台写请求要求有效来源，并拒绝跨站请求。当前配置只接受一个外部 `web_origin`，所以分享期间应使用新域名进行远程操作。[Caddy Basic Auth](https://caddyserver.com/docs/caddyfile/directives/basic_auth) · [Go 工作台入口](../../internal/agent/http.go)

**两个后台进程的启动参数**（部署记录，运行中无需再次执行）

```sh
caddy run --config "$share_dir/Caddyfile" --adapter caddyfile
cloudflared tunnel --no-autoupdate --protocol http2 --url http://127.0.0.1:18091
```

实际由分享目录内的 `start.py` 各自启动后台进程并记录 PID 和日志。公网地址属于本次隧道；Mac 休眠、断网或隧道退出时访问会中断。重新创建隧道通常会分配新域名，需要重新验证并更新 `web_origin`。本次没有设置开机自启。

## 验收结果与本机复核

| 通过公网发起的请求 | 观察到的结果 |
| --- | --- |
| 匿名访问 `/`、`/v1/status`、`/healthz` | 全部返回 `401`，响应包括 Basic 登录挑战 |
| 正确账号密码访问首页、健康端点 | 首页返回工作台内容，健康状态为 `ok` |
| 正确账号密码、工作台标记和真实公网 Origin 请求状态 API | 返回 `200`，得到执行器状态 |
| 同样的请求改为未受信任的 Origin | 返回 `403` |
| Chrome 经公网访问首页 | 品牌与副标题可见，状态 API 返回 `200` |

以上请求由部署 Mac 经公网域名发起。记录时再次运行了只读 HTTP 检查；没有从独立手机网络验收，也没有通过公网提交审批、调用模型或发送真实 IM 消息。

在部署 Mac 上，可用以下只读命令核对监听端口和上述 HTTP 结果。检查脚本从本机私有文件取凭据，终端只打印验证结果。

```sh
share_dir="$HOME/.config/agentd-share/workbench-ujwglrbv"
lsof -nP -iTCP:18091 -iTCP:8090 -sTCP:LISTEN
python3 "$share_dir/check.py" "$(cat "$share_dir/url.txt")"
```

评审配置可读同一目录的 `Caddyfile`、`start.py`、`change-origin.py` 和 `check.py`。隧道连接与代理启动记录分别在 `cloudflared.log` 和 `caddy.log`。本次公网地址和原来源分别保存在 `url.txt` 与 `previous-origin.txt`，原始配置和登录信息保留在这台 Mac 上。

## 结束分享与恢复

需要收回访问时，在部署 Mac 上执行下面的命令。此次文档复核未运行停止操作。

```sh
"$HOME/.config/agentd-share/workbench-ujwglrbv/停止公网分享.command"
```

脚本核对 PID、进程名和工作目录后，终止本次 cloudflared 与 Caddy。若 agentd 的可信来源仍是本次公网域名，就用保存的原来源恢复配置并重启 agentd。若服务繁忙导致恢复中止，公网分享已关闭，可在服务空闲后重跑该命令。恢复后可检查本机 `/healthz`，并通过原内网入口打开工作台核对状态。
