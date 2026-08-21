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

## 管理面板的 Cursor API Key 页面

管理面板（`/management.html`）是上游 Management Center 的预编译单页，本身不认识 Cursor
供应商。侧边栏的「Cursor API Key」页面（列表/新增/编辑/停用/删除 + 每个 Key 的调用
成功/失败计数和最近错误）是网关在**服务时**注入的：`internal/managementasset/cursor_api_key.go`
往 HTML 里打进导航项、SPA 路由、多语言标签和一段自包含的小组件脚本。服务器磁盘上的
`static/management.html` 永远是上游原版（面板每 3 小时自动更新也不受影响）；上游改了
打包结构导致锚点匹配不上时，网关会降级为原版面板并在日志里 warn。

> 该文件（约 2.5MB，2026-08-20 抓取的上游预编译单页，无固定版本号）不再入库：
> 它是纯上游产物且线上每 3 小时自动更新，快照里的副本没有信息量。恢复部署时
> 由面板的自动更新机制重新拉取即可。

后端是 `/v0/management/cursor-api-key` 的 GET（带 index/auth-index/success/failed/
unavailable/last_error 运行状态）+ POST（追加单个 Key）+ PUT/PATCH/DELETE；`disabled: true`
的 Key 会持久化进 config.yaml 并从路由注册里剔除。

> 历史教训：这套 UI 曾只存在于线上编译产物、没进 git，2026-08-21 部署缓存修复时被
> 覆盖丢失，后来从备份二进制 `cli-proxy-api.bak-pre-convfix-20260821-103218` 里原样
> 找回。现在代码在仓库里，重新编译部署不会再丢。

## 谁能出图

出图只有两条入口：模型 `nano-banana-pro`（`/v1/chat/completions`、`/v1/messages` 都
可以点名它），以及 `/v1/images/generations`、`/v1/images/edits`。别的模型一律出不了图。

拦截分三层：

1. Cursor 发 interaction query 要客户端批准 GenerateImage 时，非出图会话回
   rejected。
2. 非出图会话的 run request 里直接告诉模型这轮不能出图。
3. 真送来了图也丢掉，回复里换成一句"这个模型不能出图，请用 `nano-banana-pro`"。

第 3 层是唯一保证生效的：实测 Cursor 的服务端**不总是**先问客户端，`grok-4.6` 上
GenerateImage 会直接跑完并返回 success（日志里 `cursor genimage completed`），前两层
都拦不住。换句话说，上游那次生成的额度仍会被消耗，但调用方拿不到图。MVP proto 里
`AgentRunRequest` 也没有关掉这个内建工具的开关（试过 `client_supports_inline_images
=false`，照样会生成），所以只能到这一步。

模型自己经常声称"已生成"，所以回复里那句提示是必要的：否则用户只会看到一段说图已
经出好了的文字，外加一个指向 Cursor 侧工作区、根本打不开的本地路径（这个死链接现在
也会被剥掉）。

模型对外只叫 `nano-banana-pro`；以前的 `cursor-image` / `image` 都不再注册，
`/v1/models` 里没有它们，点名会被当成未知模型拒掉。

## 2K / 4K 自动超分

Cursor 的 GenerateImage 只收一段文字描述，协议里没有尺寸参数，出图固定在 1536×1024
上下（约 1.5 MP）。所以"要 4K"这件事上游做不到，只能网关自己放大。

识别：看这一轮的用户提示（以及 `/v1/images/*` 的 `size` 参数），命中就取长边像素——
`2K`/`QHD`/`1440p`/`2560×1440` → 2560，`4K`/`8K`/`UHD`/`2160p`/`3840x2160` → 3840，
写死的 `3000x2000` 这种按原样取（上限 4096）。`1024x1024` 及以下不算"要更大"，直接跳过。
`2k people` 这类计数写法有一份名词黑名单挡掉；提示里完全不提尺寸就什么都不做。

放大：Catmull-Rom 三次卷积分离式重采样（先行后列），再过一遍轻度 unsharp mask 补回
放大损失的局部对比度。纯 Go 实现，没引入新依赖——这台机器上没有 GPU，也没有
realesrgan / waifu2x / ffmpeg / ImageMagick，所以不是神经网络超分，是高质量重采样。

边界：最多放大 4 倍（再多就只是糊，不如给原图）；已经够大的图原样返回；PNG 超过 6 MB
时改存 JPEG q92（实测 1536×1024 → 3840×2560 的 PNG 有 11 MB，JPEG 只有 1.2 MB，客户端
要先下完才能显示）。耗时：2K 约 0.65 s，4K 约 1.5 s，相对 20~30 s 的出图可以忽略。

## 生成图片的对外地址

生成的图片由网关自己托管在 `GET /media/<随机名>.png`（无需 API key，URL 本身即凭据，
24 小时过期）。聊天回复里给出的是这个 URL，而不是 data URL——客户端的 markdown 净化器
（harden-react-markdown / streamdown）一律拒绝 `data:`，表现就是 `[Image blocked: …]`。

路径里不出现供应商名字：链接每次出图都会展示给用户。`/cursor-images/` 是换名之前的
前缀，仍然照常服务，旧对话里的链接在字节过期前都还能打开。

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
  `![](https://0fcc5ed6.sslip.io/media/xxx.png)`，网关把 base64 解码后
  的原始字节以 `Content-Type: image/png` 返回，客户端请求到的就是图片本身。
  主机名是本机 IP 的十六进制写法（`0f cc 5e d6` = 15.204.94.214），走
  sslip.io 通配 DNS 解析到同一台机器，单独签了 Let's Encrypt 证书，所以正文
  里看不出地址，客户端又能正常校验证书。
  图片路由 `/media/:name`、`/v1/images/:name` 和旧的 `/cursor-images/:name`
  都不走 API key 中间件（`<img>` 不会带凭证），文件名是 128 bit 随机值，
  URL 本身就是凭证；同时带 `Access-Control-Allow-Origin: *` 和
  `Cross-Origin-Resource-Policy: cross-origin`，客户端要 fetch 成 blob 也读得到。
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
- 80 端口对 `/media/` 和旧的 `/cursor-images/` 都做 301，把明文发出去的链接导到
  受信任域名。
- 续期由 `certbot.timer` 负责，`renewal-hooks/deploy/reload-nginx.sh` 在续期后 reload nginx。
  换成自有域名后，改 drop-in 里的地址 + 新签一张证书即可。

## 目录对照

```
opt/cli-proxy/                    # 服务器 /opt/cli-proxy（运行时主目录）
  config.yaml                     # 网关生效配置（已脱敏）
  adapter/                        # Claude Code 适配器（adapter.mjs / package*.json）
  agents/claude-account.env       # 账号 agent 环境变量（已脱敏）
  bin/                            # 项目自带脚本（Python 回环代理、egress-guard.sh）
  etc/cursor-proxy.env            # Cursor 出口代理环境变量（已脱敏）
etc/systemd/system/               # 服务器上的 systemd 单元及 drop-in
etc/nginx/sites-available/        # nginx 反代站点配置（线上生效的是 sites-enabled 下的同名普通文件）
etc/letsencrypt/renewal-hooks/    # 证书续期后的 nginx reload 钩子
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
- `/opt/cli-proxy/static/management.html`（约 2.5MB 上游预编译面板，见上文说明）
- `/opt/cli-proxy/adapter/sdk-diag.mjs`（一次性 SDK 连通性诊断脚本，非运行时依赖）
- `~/modeb-deploy/` 原始部署包（早期部署脚本与配置/systemd 模板，内容已被
  `opt/`、`etc/` 下的生效副本取代，线上以生效副本为准）
- 历史 `config.yaml.bak-*` 备份、`~/staging` 与 `~/cachetest` 下的测试二进制
