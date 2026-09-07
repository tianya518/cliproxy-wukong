# wukong × CLIProxyAPI（薄 fork · 进程内原生 provider）

本目录是 **wukong 网页逆向**（ChatGPT / Grok 的网页协议）作为 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)
**进程内原生 provider** 融合后的产品代码。整个仓库是 CLIProxyAPI 的一个薄 fork：
上游原样保留在仓库根（`sdk/`、`internal/`、`cmd/` …），我们的代码集中在 `wukong/`，
只在上游打了一处极小补丁，就让 `chatgpt-web` / `grok-web` 成为和内置 provider 一样的
一等公民——热重载中天然存活，凭证进统一池轮换/冷却，产物代理与账号管理挂在同一个
网关端口上，**对外只有一个入口**。

## 仓库布局

```
<repo root>/                         # 模块 github.com/router-for-me/CLIProxyAPI/v7（CLIProxyAPI 的 fork）
├── sdk/ internal/ cmd/ ...          # 上游原样
│   └── sdk/cliproxy/native_provider.go   # 补丁①：进程内 provider 注册表（新文件）
│                                          # 补丁②③：service_executors.go / service_models.go 各一行钩子
└── wukong/                          # 我们的代码（属于 fork 模块，import 前缀 .../v7/wukong）
    ├── sentinel/                    # ChatGPT 网页协议库
    ├── grok/                        # Grok.com 网页协议库
    ├── server/                      # 对话内核 Engine + 产物代理 + 账号管理（gin 聊天服务已裁掉）
    ├── cliproxy/                    # glue：把逆向包成 cliproxy provider + 网关入口
    │   └── cmd/wukong-gateway/      # 网关二进制（唯一对外入口）
    │   └── cmd/e2e/                 # 对真实上游的端到端回归
    ├── cmd/grok-live/               # grok.com 联调工具（协议维护用）
    ├── cmd/stream-capture/          # SSE 抓流工具（协议维护用）
    ├── panel/              # fork 版管理面板（management.html 内嵌进二进制）
    └── docs/                        # 协议抓包参考（IMAGE_FLOW_CAPTURE / PROTOCOL_BASELINE）
```

补丁边界只有那一处（注册表新文件 + 两行钩子）；`wukong/` 全是新目录，上游永不触碰。

## 构建 & 运行（单一入口）

```powershell
# 在仓库根构建网关二进制
go build -o scp.exe ./wukong/cliproxy/cmd/wukong-gateway

# 配置（改掉里面的 api-keys）
cp wukong/cliproxy/config.example.yaml config.yaml

$env:CLIPROXY_CONFIG = "config.yaml"
$env:CHATGPT_FILE    = "chatgpt.json"   # 仅启动时一次性迁到 auth-dir；日常灌号走 /chatgpt 或 management API
$env:GROK_FILE       = "grok.json"      # 仅启动时一次性迁到 auth-dir；日常灌号走 /grok
# 生图链接默认指向网关自身（config 的 host:port）。对外部署时设成末端可达地址：
# $env:ARTIFACT_BASE_URL = "https://your.domain"
# 临时模式默认关闭，ChatGPT / Grok 新会话都是普通会话（进官网历史、可用账号记忆）。
# 想隔离账号级跨会话记忆再打开；GROK_TEMP_MODE 没设时沿用 TEMP_MODE：
# $env:TEMP_MODE      = "true"   # ChatGPT：history_and_training_disabled；生图与项目对话自动豁免
# $env:GROK_TEMP_MODE = "true"   # Grok：session.create 带 is_temporary + disable_memory
./scp.exe
```

**只监听一个端口**（config 的 `host:port`，默认 `:8317`）：

- OpenAI / Claude / Gemini 三套协议入口由 cliproxy 提供，走网关 `api-keys` 鉴权；
- 生图/沙箱产物代理 `/api/image/proxy`、`/api/pdf/proxy`、静态图片 `/images`，以及账号
  管理 `/chatgpt`（旧名 `/tokens`）、`/grok`，由 wukong 直接挂在同一个 gin 引擎的**根路由、
  免 api-key**（图片链接要能被末端客户端直接取；灌号 Grok 会热更新 `grok-web`）。

模型名：ChatGPT 侧 `chatgpt-web/gpt-5-*`（运行时从官网 `/backend-api/models` 拉取，可配强度的
slug 另暴露 `-standard` / `-extended`）和 `chatgpt-web/dall-e-3`（生图触发名，不在官网目录里，
后端以 `picture_v2` 直调图像工具）；Grok 侧 `grok-web/grok-chat-*` / `grok-web/grok-imagine-*`。

前缀走的是 cliproxy 自带的凭证 `prefix` 机制：`/chatgpt/upload`、`/grok/upload` 与旧文件迁移写入的
凭证默认带 `prefix: chatgpt-web` / `grok-web`，模型因此注册为 `<prefix>/<model>`，请求时自动剥掉；
config 里 `force-model-prefix: true` 让无前缀旧名（`gpt-5-6-thinking`、`dall-e-3`）不再出现，
这样和 `codex` / `xai` 内置 provider 的官方 API 模型（`gpt-5.5`、`gpt-image-2`、`grok-4.6`）在
`/v1/models` 里一眼分开。升级前灌的老凭证文件没有 `prefix` 字段，在面板凭证卡里补上，或手工往
JSON 里加 `"prefix": "chatgpt-web"`（Grok 为 `"grok-web"`）；想换前缀也在这里改。

## 产物存储：图片 / 文件 / 视频链接

网页通道生成的产物（ChatGPT 生图、Code Interpreter 沙箱文件、Grok 图片与视频）对外统一是
`<ARTIFACT_BASE_URL>/files/<id>.<ext>`，不再有 `assets.grok.com` 直链（需要登录 cookie，匿名 403），
也不再依赖内存会话（旧的 `/api/image/proxy` 两小时或重启后 404）。设计与分期见
`docs/ARTIFACT_STORE_PLAN.md`，实现在 `server/artifact_*.go`、`cliproxy/artifact_fetchers.go`。

两层结构：

- **映射表**（`ARTIFACT_DIR/index.jsonl`，追加写、不清理）：每个产物属于哪个 provider、哪个凭证
  （`auth-dir` 文件名）、在上游怎么定位（ChatGPT 的 `conv_id + file_id` / `msg_id + sandbox_path`，
  Grok 的原始 URL）。生成侧先登记再发链接。**要纳入备份**——它丢了，未缓存的产物就无法回源。
- **磁盘缓存**（`ARTIFACT_DIR/<id>.<ext>`）：图片在响应前同步下载，视频 / 沙箱文件后台预下载；
  受 `ARTIFACT_MAX_TOTAL_MB` 约束按最旧优先清理，**只删字节不删映射**。

访问 `GET /files/<id>.<ext>` 三级取回：缓存命中直出（immutable、支持 Range）→ 缓存缺失按映射用该凭证
回源并写回（响应头 `X-Artifact-Source: upstream`）→ 映射也没有则 404。回源**不跨账号**：ChatGPT 与
Grok 的文件都只对创建它的账号开放，凭证被删就 502。旧的 `/api/image/proxy`、`/api/pdf/proxy` 变成兼容
别名：映射命中走同一条链，命中不了才退回会话代理并顺手回填。

环境变量（都有默认值，不配也能跑）：

| 变量 | 默认 | 说明 |
|---|---|---|
| `ARTIFACT_DIR` | `artifacts` | 缓存目录（相对工作目录） |
| `ARTIFACT_INDEX_PATH` | `<ARTIFACT_DIR>/index.jsonl` | 映射表 |
| `ARTIFACT_PUBLIC_PATH` | `/files` | 对外路径前缀 |
| `ARTIFACT_MAX_TOTAL_MB` | `2048` | 缓存总大小上限，`0` 不限 |
| `ARTIFACT_MAX_AGE_DAYS` | `0` | 缓存按天清理，`0` 不限 |
| `ARTIFACT_FETCH_TIMEOUT_SEC` | `60` | 访问时回源单次超时 |
| `ARTIFACT_VIDEO_TIMEOUT_SEC` | `120` | Grok 视频后台预下载超时（超时不影响链接，访问时再回源） |
| `ARTIFACT_BASE_URL` | 网关自身 | 链接前缀，对外部署必须设成末端客户端可达的地址 |

`/files/*` 与旧的产物代理一样免 api-key，靠不可枚举的 ID（官网 `file_id` / 资源 UUID / 哈希）保护。

## 账号池：现状与刷新蓝图

**已统一。** ChatGPT / Grok 账号启动时注册进 cliproxy 的凭证池，作为一个
池子参与**轮换、冷却、失败重试**（已实测：上游 401 会对该凭证记冷却，其余继续服务；
热重载后凭证与模型仍在）。

**灌号、状态口、模型目录都走 Manager / auth-dir。**
`POST /chatgpt/upload` 写成 `auth-dir/chatgpt-web-<id>.json`，
`POST /grok/upload` 写成 `auth-dir/grok-web-<id>.json`。
启动时先读 auth-dir，再把旧的 `chatgpt.json` / `grok.json` 一次性迁过去。
ChatGPT 刷新和 Grok Clearance 更新都写回同一目录。`grok-live` 仍可用 `-file` 直打协议。

**Grok 额度。** `GET /grok/quota` 拉全部账号的 grok.com 额度，`?id=<账号名或 auth-dir
文件名>` 只拉一个。`GET /grok/check` 默认顺带带上，`?quota=0` 只验会话。每个账号两层：

- `windows`：`/rest/rate-limits` 的 `auto` / `fast` 聊天滚动窗口（剩余 / 总数，平时没有重置
  时间，只有被限流时上游才带 `waitTimeSeconds` → `reset_at`），加 `image` / `image_pro` /
  `image_edit` / `video` / `video_720p` 的 Imagine 可用性标志。
- `billing`：grok.com 设置页「使用量」那套订阅额度，即 `SuperGrok Heavy 周限额 6%` 那种数字。
  走同源 gRPC-web（`grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig` +
  `prod_mc_billing.ConsumerUiSvc/GetRemainingResets`，`grok/billing.go` 手写帧 + protowire 解码，
  不需要 Statsig 签名）：`usage_percent` 是当期**已用**百分比，`period_type` / `period_end`
  是周期与重置时刻，`products[]` 按产品线（imagine / chat / voice …）拆分，`resets[]` 是尚未
  过期的「用量限额重置」券，`prepaid_balance_cents` 是额外额度余额。拿不到只填 `billing_error`，
  不影响 `windows`。

xAI 那张卡（Grok CLI OAuth）走的是 cli-chat-proxy 的账单口，和这里不是一套账号。

## 管理面板（fork 版 CPAMC）

网关服务的 `/management.html` 不是上游 release 里的那份，而是
[Cli-Proxy-API-Management-Center](https://github.com/router-for-me/Cli-Proxy-API-Management-Center)
的 fork 构建，多了 `grok-web` 的额度组件（`src/features/quota/providers/grok-web/`，调网关自己的
`/grok/quota?id=<文件名>`），以及 `grok-web` / `chatgpt-web` 的显示名、图标、配色。

构建产物 `wukong/panel/management.html` 通过 `go:embed` 随二进制发布。启动时 `panel.Install`
把它写到 SDK 服务面板的 static 目录，并把 `remote-management.disable-auto-update-panel`
钉成 true——上游更新器 3 小时一轮会用 release 的 hash 比对并覆盖本地文件，不钉住就会被
换回不认识这两个 provider 的官方版。**config.yaml 里也要写上这个开关**（见
`config.example.yaml`），否则热重载后内存里的钉子就没了；启动日志会提示。

重建面板（前端源码在同级目录 `../Cli-Proxy-API-Management-Center`，需要 Node 22+）：

```powershell
cd ../Cli-Proxy-API-Management-Center
npm install
npx bun@1.3.14 test          # 上游测试用 bun:test，没装 bun 就用 npx 拉
$env:VERSION = "wukong-" + (git rev-parse --short HEAD)
npm run build  # 产物 dist/index.html（单文件）
Copy-Item dist/index.html ../cliproxy-wukong/wukong/panel/management.html
cd ../cliproxy-wukong && go test ./wukong/panel/ && go build -o scp.exe ./wukong/cliproxy/cmd/wukong-gateway
```

跟进上游面板：在 fork 里 `git rebase` 上游 main，冲突面集中在 6 个枚举点
（`providers/types.ts`、`providers/index.ts`、`constants.ts` 的 `QUOTA_TAB_ORDER`、
`logic.ts`、`QuotaPage.tsx`、`useQuotaStore.ts`）和 `authFiles/constants.ts` 的
`QuotaProviderType`；`grok-web/` 目录本身是新文件不冲突。

## 跟进上游（fork 维护）

本仓库是 CLIProxyAPI 的 fork，补丁在分支 `wukong-patches`。升级上游：

```bash
git fetch --tags origin        # origin 指向 router-for-me/CLIProxyAPI
git rebase v7.x.y
go build ./... && go test ./wukong/...
go run ./wukong/cliproxy/cmd/e2e   # 起服务后端到端回归
```

冲突面：`native_provider.go` 是新文件不冲突；两行钩子（`service_executors.go` /
`service_models.go`）偶尔需手工对齐；`go.mod` 因为扁平合并并入了 wukong 的依赖，上游改
`go.mod` 时会冲突——取上游版本后 `go mod tidy` 收口即可。契约破坏会以编译错误或
`wukong/cliproxy` 的 `TestExecutorSatisfiesInterfaces` 失败暴露。

## 参考

- `wukong/docs/IMAGE_FLOW_CAPTURE.md`：官网生图协议抓包记录
- `wukong/docs/PROTOCOL_BASELINE.md`：协议基线
- `wukong/cmd/grok-live`、`wukong/cmd/stream-capture`：官网/Grok 改协议时重新抓包的工具
