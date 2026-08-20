# CPA 生产服务器部署快照

本目录是服务器 `15.204.94.214` 上 CPA（CLIProxyAPI Mode-B relay 网关）实际部署的完整快照，
于 2026-08-20 从线上环境采集。目录结构与服务器上的绝对路径一一对应。

> 服务器上没有源码检出，只有编译产物和运行时文件；本仓库的 `modeb-relay` 分支即为对应源码。

## 服务器上的服务拓扑

| systemd 服务 | 作用 |
| --- | --- |
| `cli-proxy-api.service` | CPA 网关主进程（`/opt/cli-proxy/bin/cli-proxy-api`，监听 127.0.0.1:8317） |
| `cli-proxy-local-relay.service` | 本地回环代理（`cursor_connect_proxy.py`，127.0.0.1:18006，drop-in 覆盖为直连模式） |
| `relay-adapter@.service` | 每账号 Claude Code 适配器（Node，Unix socket `/run/relay/agent-<account>.sock`） |
| `relay-egress-guard.service` | netfilter 出口防护（网关 uid 禁止非回环出网） |
| `rqlite-relay.service` | rqlite 亲和/封禁状态存储（127.0.0.1:4001） |
| `nginx`（sites: `cli-proxy`, `cli-proxy-http`, `cli-proxy-tls-hostname`） | 80/443 反代到 127.0.0.1:8317，SSE 长连接 1800s 超时，透传 `X-Forwarded-Proto` |
| `certbot.timer` | 续期 `15-204-94-214.sslip.io` 的 Let's Encrypt 证书 |

> **改 nginx 时注意**：`/etc/nginx/sites-enabled/cli-proxy` 和 `cli-proxy-http` 是**普通文件，
> 不是指向 sites-available 的软链**（只有 `grok-dashboard` 是软链）。改 `sites-available`
> 不会生效，必须改 `sites-enabled` 下的那一份。备份文件也不能留在 `sites-enabled/`，
> 否则会被 `include sites-enabled/*` 加载并报 duplicate default server；本机备份放在
> `/etc/nginx/site-backups/`。

## 生成图片的对外地址

Cursor 生成的图片由网关自己托管在 `GET /cursor-images/<随机名>.png`（无需 API key，
URL 本身即凭据，72 小时过期）。聊天回复里给出的是这个 URL，而不是 data URL——客户端的
markdown 净化器（harden-react-markdown / streamdown）一律拒绝 `data:`，表现就是
`[Image blocked: …]`。

对外地址默认取自入站请求的 `Host` + `X-Forwarded-Proto`，但本机 443 的默认证书是给裸 IP
签的自签证书：客户端渲染器/Node 侧再去抓这个图就会报 `DEPTH_ZERO_SELF_SIGNED_CERT`。
因此这台机器上：

- `15-204-94-214.sslip.io` 解析到本机 IP，用 webroot（`/var/www/acme`）签了 Let's Encrypt
  证书，由 `sites-available/cli-proxy-tls-hostname` 这个 vhost 提供，任何客户端都信任；
  裸 IP 的自签 vhost 原样保留，现有 API 客户端不受影响。
- drop-in `cli-proxy-api.service.d/public-base-url.conf` 把 `CPA_PUBLIC_BASE_URL` 固定为
  `https://15-204-94-214.sslip.io`，并把图片缓存目录指到 `/opt/cli-proxy/image-cache`
  （服务用户 `cliproxy` 没有 home，且 `PrivateTmp=true` 会在重启时清空 /tmp）。
- 生成图只保留 24 小时：网关自带清理协程，启动时先扫一遍，之后每小时扫一次，
  删掉 `/opt/cli-proxy/image-cache` 里超过一天的文件，不需要 cron 或 systemd timer。
- 生成图的交付方式由 `CPA_IMAGE_DELIVERY` 决定，本机取 `link`：正文里是
  `![](https://0fcc5ed6.sslip.io/cursor-images/xxx.png)`，网关把 base64 解码后
  的原始字节以 `Content-Type: image/png` 返回，客户端请求到的就是图片本身。
  主机名是本机 IP 的十六进制写法（`0f cc 5e d6` = 15.204.94.214），走
  sslip.io 通配 DNS 解析到同一台机器，单独签了 Let's Encrypt 证书，所以正文
  里看不出地址，客户端又能正常校验证书。
  图片路由 `/cursor-images/:name` 和 `/v1/images/:name` 都不走 API key 中间件
  （`<img>` 不会带凭证），文件名是 128 bit 随机值，URL 本身就是凭证；
  同时带 `Access-Control-Allow-Origin: *` 和 `Cross-Origin-Resource-Policy:
  cross-origin`，客户端要 fetch 成 blob 也读得到。
  其余取值：`relative`（只给 `/v1/images/xxx.png`）、`base64`（data URL 写进
  markdown）。
- 三种"不暴露地址"的交付都实测失败过，别再走回头路：
  - markdown 里塞 `data:` —— harden-react-markdown / streamdown 默认
    `allowDataImages=false`，一律换成 `[Image blocked: …]`。
  - Anthropic assistant `content_block.type=image` —— assistant content union
    里没有 image，严格校验 SSE 的客户端直接
    `Type validation failed … No matching discriminator`，整轮对话中断。
  - 无主机名的相对路径 `/v1/images/xxx.png` —— 服务端返回 200 image/png，但
    客户端渲染器按自己的 app origin 去解析，请求根本没打到网关，表现为**裂图**。
    所以 img src 必须是绝对 URL，能做的只是让这个 URL 不写成 IP 的样子。
- 想让图片彻底不带地址，目前没有可行解，两条路都试过并被客户端否掉：
  - markdown 里塞 `data:` —— harden-react-markdown / streamdown 默认
    `allowDataImages=false`，一律换成 `[Image blocked: …]`，跟前缀白名单无关。
  - Anthropic assistant 的 `content_block.type=image` —— assistant content
    union 里根本没有 image 这一项（只有 text / thinking / tool_use /
    各类 *_tool_result），严格校验 SSE 的客户端会直接
    `Type validation failed … No matching discriminator`，整轮对话报错中断。
    所以 assistant 回复只能"指向"一张图，不能"携带"一张图。
- 80 端口对 `/cursor-images/` 做 301，把换证书之前发出去的旧链接也导到受信任域名。
- 续期由 `certbot.timer` 负责，`renewal-hooks/deploy/reload-nginx.sh` 在续期后 reload nginx。
  换成自有域名后，改 drop-in 里的地址 + 新签一张证书即可。

## 目录对照

```
opt/cli-proxy/                    # 服务器 /opt/cli-proxy（运行时主目录）
  config.yaml                     # 网关生效配置（已脱敏）
  adapter/                        # Claude Code 适配器（adapter.mjs / sdk-diag.mjs / package*.json）
  agents/claude-account.env       # 账号 agent 环境变量（已脱敏）
  bin/                            # 项目自带脚本（Python 回环代理、egress-guard.sh）
  etc/cursor-proxy.env            # Cursor 出口代理环境变量（已脱敏）
  static/management.html          # 管理面板单页 UI
etc/systemd/system/               # 服务器上的 systemd 单元及 drop-in
etc/nginx/sites-available/        # nginx 反代站点配置（线上生效的是 sites-enabled 下的同名普通文件）
etc/letsencrypt/renewal-hooks/    # 证书续期后的 nginx reload 钩子
home/ubuntu/modeb-deploy/         # 原始部署包（setup.sh / egress-guard.sh / 配置模板 / systemd 模板）
home/ubuntu/cachetest/            # 缓存命中对比测试脚本（run_cachetest_gw.sh，已脱敏）
```

## 已脱敏内容（占位符 `REDACTED_*`）

真实值仍保留在服务器原文件中，恢复部署时需回填：

| 文件 | 脱敏项 |
| --- | --- |
| `opt/cli-proxy/config.yaml` | 管理面板 bcrypt 哈希、网关 api-key、2 个 Cursor API key |
| `opt/cli-proxy/agents/claude-account.env` | Oxylabs 住宅代理账号密码、`CLAUDE_CODE_OAUTH_TOKEN` |
| `opt/cli-proxy/etc/cursor-proxy.env` | 本地代理账号密码 |
| `etc/systemd/system/cli-proxy-api.service.d/cursor-proxy.conf` | 本地代理账号密码 |
| `home/ubuntu/cachetest/run_cachetest_gw.sh` | 本地代理账号密码 |

## 未包含在快照中的内容

- `/opt/cli-proxy/bin/` 下的编译二进制及全部 `.bak` 备份（约 11GB；源码即本仓库，可重新编译）
- `/opt/cli-proxy/auths/` OAuth 凭证文件、`cert.pem` / `key.pem`（私钥凭证，不入库）
- `/opt/cli-proxy/logs/`（约 261MB 运行日志）、`/opt/cli-proxy/rqlite/`（数据库状态）
- `adapter/node_modules/`（约 563MB，可由 `package-lock.json` 重装）
- 历史 `config.yaml.bak-*` 备份、`~/staging` 与 `~/cachetest` 下的测试二进制
