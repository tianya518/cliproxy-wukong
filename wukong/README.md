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
./scp.exe
```

**只监听一个端口**（config 的 `host:port`，默认 `:8317`）：

- OpenAI / Claude / Gemini 三套协议入口由 cliproxy 提供，走网关 `api-keys` 鉴权；
- 生图/沙箱产物代理 `/api/image/proxy`、`/api/pdf/proxy`、静态图片 `/images`，以及账号
  管理 `/chatgpt`（旧名 `/tokens`）、`/grok`，由 wukong 直接挂在同一个 gin 引擎的**根路由、
  免 api-key**（图片链接要能被末端客户端直接取；灌号 Grok 会热更新 `grok-web`）。

模型名：ChatGPT 侧 `gpt-5-*` / `o3` / `dall-e-3`（运行时从官网目录拉取）；Grok 侧
`grok-chat-*` / `grok-imagine-*`。与官方 `codex` / `xai` 内置 provider 不重叠。

## 账号池：现状与刷新蓝图

**已统一。** ChatGPT / Grok 账号启动时注册进 cliproxy 的凭证池，作为一个
池子参与**轮换、冷却、失败重试**（已实测：上游 401 会对该凭证记冷却，其余继续服务；
热重载后凭证与模型仍在）。

**灌号、状态口、模型目录都走 Manager / auth-dir。**
`POST /chatgpt/upload` 写成 `auth-dir/chatgpt-web-<id>.json`，
`POST /grok/upload` 写成 `auth-dir/grok-web-<id>.json`。
启动时先读 auth-dir，再把旧的 `chatgpt.json` / `grok.json` 一次性迁过去。
ChatGPT 刷新和 Grok Clearance 更新都写回同一目录。`grok-live` 仍可用 `-file` 直打协议。

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
