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
| `nginx`（sites: `cli-proxy`, `cli-proxy-http`） | 80/443 反代到 127.0.0.1:8317，SSE 长连接 900s 超时 |

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
etc/nginx/sites-available/        # nginx 反代站点配置
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
