# 产物存储（ArtifactStore）实施计划

状态：阶段 1、阶段 2 均已上线并验收通过（2026-09-07，见第 10 节）。

网关把 ChatGPT / Grok 网页通道生成的图片、文件、视频交给标准 OpenAI 客户端（Open WebUI 等）时，
目前全部依赖"内存会话 + 现拉现转"或"上游直链"。本计划引入服务器侧的产物存储层：**一张持久化的
映射表**记录每个产物"属于哪个 provider、哪个凭证、在上游怎么定位"，**磁盘作为缓存**在生成时写入、
缺失时按映射回源补回；同时对齐 cliproxy Codex 通道的图片内联格式。结果是产物链接不依赖内存会话、
账号 cookie、网关重启或磁盘清理，客户端只访问网关、不需要自己能连上游，多轮改图有视觉上下文。

## 1. 背景：现状与问题

| # | 产物 | 现在怎么给客户端 | 问题 |
|---|------|------------------|------|
| P1 | Grok 图片 / 视频 | markdown 里直接放 `https://assets.grok.com/...` 直链（`wukong/grok/engine.go: imageMarkdown`、`VideoURL`） | 该域名需要 Grok 登录 cookie，匿名请求 **403**（2026-09-06 实测）。任何标准客户端从第一秒起就是裂图；视频还用了 `![]()` 图片语法，`<img>` 不会播放 mp4 |
| P2 | ChatGPT 图片 | `![](…/api/image/proxy?conv_id&file_id)`，代理时从 `SessionManager` 取会话 client 用其账号 token 去官网下载（`wukong/server/handler_proxy.go`） | 会话 TTL 120 分钟（`SESSION_TTL_MINUTES`）、纯内存。到期或网关重启后链接 **404** |
| P3 | ChatGPT 沙箱文件（Code Interpreter 生成的 pdf / docx / csv / txt） | `[name](…/api/pdf/proxy?conv_id&msg_id&sandbox_path)`，同上 | 同 P2 |
| P4 | 下一轮上下文 | 标准客户端不回传 `conversation_id`，网关每轮新开官网会话并把历史展平成文本（`engine.go: prepare` / `flattenHistory`） | 上一轮 assistant 消息里的图片只是一行 URL 文本，上游模型看不见图，"把刚才那只猫换成狗"靠猜 |
| P5 | 续接轮次 | 回传 `conversation_id` 时按整个会话的"图槽"重建列表（`RebuildImageFileIDsFromSlots`） | 第二轮响应把第一轮的 4 张旧图也带回来（4 旧 + 1 新），逐条渲染的客户端会看到旧图重复 |

上游 CLIProxyAPI 对生成产物**零持久化**：图片内联 base64，视频用 `video_id → 凭证` 内存绑定（3h TTL）
现拉现流，没有文件类端点。这套做法成立的前提是官方产物要么内联、要么有客户端可直接访问的限时 URL。
网页逆向不满足这个前提：ChatGPT 的图和文件要账号 token 才能从 `/backend-api/files/download` /
`interpreter/download` 取，Grok 的资源要 cookie。网关必须做中间人，中间人有两种做法：
"每次代理（依赖产物 → 凭证的绑定表）"或"落盘"。本计划两者合并：绑定表持久化作为底，磁盘作为缓存。
纯代理的问题是每次浏览都打上游（Open WebUI 重开对话、换设备都会重取，Grok 反复请求有触发反爬的风险）、
链接寿命绑死在账号 cookie / token 上；纯落盘的问题是被 LRU 清掉后链接就死。合并后互相补位。

### 1.1 已核正的一处判断

之前认为"ChatGPT 沙箱文件官网侧过期快，事后回源拉不到"，这是把两件事混在一起了：

- **OpenAI API 的 Responses `containers`**（code interpreter 工具）：容器空闲 20 分钟销毁，文件随之消失。
  这是 API 产品的行为，与 chatgpt.com 无关。
- **chatgpt.com 网页**：沙箱**执行环境**的状态会过期（模型后续无法再读到旧文件、说"文件已不在沙箱"），
  但**挂在消息上的输出文件**在消息定稿时被复制到持久存储，`interpreter/download` 每次换取一个新的签名
  直链，只要会话存在就能下载。这与"昨天网页生成的 doc 今天仍能下载"的观察一致。
  签名直链本身是短时效的，但可以随时重新换取。

因此 P3 的真实约束不是"官网很快没了"，而是与 P2 相同的"下载需要该账号 token + 会话绑定"。
落盘对 P3 仍是最简单的统一做法，但不再是唯一选项；临时模式（`TEMP_MODE=true`）下的文本会话在官网
约 30 天后清理，届时文件随会话消失，落盘可以覆盖这一点。

## 2. 目标与非目标

目标：

1. 所有网页通道产物（ChatGPT 图片 / 沙箱文件，Grok 图片 / 视频）的链接不依赖内存会话、网关重启，
   长期可用；磁盘缓存被清理后能按映射自动回源补回；账号失效后已缓存的产物仍可访问。
2. 任何上游原生链接（`assets.grok.com`、官网签名直链）都不向客户端透传；客户端只需要能访问网关，
   出站由网关按现有 `PROXY_URL` 配置走代理。
3. 图片额外以 `choices[0].message.images[]`（data URL）内联，与 cliproxy Codex 通道格式一致。
4. 标准客户端（不回传 `conversation_id`）多轮改图时，上游模型能看到上一轮生成的图。
5. 回传 `conversation_id` 的续接轮次只返回本轮新增的图。

非目标：

- 不做鉴权体系变更：`/files/*` 与现有 `/api/image/proxy` 一样免 api-key，靠不可枚举 ID 保护。
- 不做 CDN / 对象存储适配；本地磁盘即可，预留接口。
- 不改上游 `sdk/` `internal/` 任何文件。
- 不为标准客户端做"消息哈希 → 官网会话"的自动映射（方案 E 的演进项，另议）。

## 3. 总体设计

生成侧：先登记映射，再（尽量）把字节写进缓存，链接只指向网关。

```
  ChatGPT 生图 ──► 收齐 file_id ──┐
  ChatGPT 沙箱 ──► msg_id+path ───┼──► index.Record(id, provider, authID, locator, mime, name)
  Grok 图/视频 ──► 资源 URL ──────┘            │
                                              ├──► 图片：立即用该凭证下载 → cache.Put（内联 base64 也靠这一份）
                                              └──► 视频 / 大文件：后台下载 → cache.Put，不阻塞响应
                                                        │
                        markdown / images[]  ◄──────────┘   ![](BASE/files/<id>.png)  [name](BASE/files/<id>.pdf)
                                                            [Generated Video](BASE/files/<id>.mp4)
```

访问侧：三级取回。

```
  GET /files/<id>.<ext>
      │
      ├─ 1. cache 命中 ───────────────────────────► 直接回（immutable、支持 Range）
      │
      ├─ 2. cache 未命中，index 命中 ─► 按 authID 从凭证池取当前有效凭证
      │                                 → provider fetcher 用该凭证 + 出站代理去上游取
      │                                 → 边流给客户端边写回 cache
      │
      └─ 3. index 也没有 ─────────────────────────► 404

  GET /api/image/proxy?conv_id&file_id      ── 兼容旧链接：换算成 id 后走同一条链
  GET /api/pdf/proxy?conv_id&msg_id&path    ── 同上；index 里没有时才退回旧的会话逻辑
```

设计要点：

- **映射是底，磁盘是缓存。** `index` 按条持久化、不设上限、不被清理；`cache` 受 `ARTIFACT_MAX_TOTAL_MB`
  约束做 LRU，**只删字节不删映射**，被清掉的文件下次访问自动回源补回。
- **链接只指向网关，且永远可以先发。** 生成侧登记映射后就能给出 `/files/<id>`，即便字节还没下完；
  访问时缺什么补什么。图片仍在响应前下载完（内联 base64 需要），视频不阻塞。
- **映射里存凭证 ID，不存 token。** 回源时从凭证池解析当前有效凭证，token 刷新、cookie 更新都不影响。
- **不透明 ID，不做路径改写。** 不把 `assets.grok.com/users/<uid>/...` 之类的上游路径映射成网关路径，
  避免暴露上游结构和用户 ID，也不必靠路径反推账号。
- **一套 store 三类共用**：图片、文件、视频只有扩展名、大小和"是否阻塞下载"不同。
- **图片双路**：网关链接（所有客户端）+ `images[]` 内联（Open WebUI 等认 OpenRouter 约定的客户端）。
- **Grok 库保持通用**：`wukong/grok` 不知道 store 的存在，只暴露一个 URL 改写钩子和一个带认证的下载方法。

## 4. 组件与改动

### 4.1 ArtifactStore = 持久映射（index）+ 磁盘缓存（cache）

新文件 `wukong/server/artifact_index.go`、`wukong/server/artifact_cache.go`、`wukong/server/artifact_store.go`。

```go
// 映射条目：产物是谁的、在上游怎么找。持久化，不清理。
type ArtifactRecord struct {
    ID        string    // 不透明主键，同文件名主体
    Ext       string    // png/jpg/webp/mp4/pdf/...
    Mime      string
    Name      string    // 展示名（沙箱文件原名），可空
    Provider  string    // chatgpt-web / grok-web
    AuthID    string    // 凭证 ID（auth-dir 文件名），回源时据此从凭证池取当前有效凭证
    Locator   ArtifactLocator
    CreatedAt time.Time
}

// 上游定位符：三种来源各填自己的字段。
type ArtifactLocator struct {
    ConvID      string // chatgpt-web：图片与沙箱文件都需要
    FileID      string // chatgpt-web 图片：files/download/{file_id}
    MessageID   string // chatgpt-web 沙箱文件：interpreter/download?message_id=&sandbox_path=
    SandboxPath string
    URL         string // grok-web：assets.grok.com / imagine-public.x.ai 原始 URL
}

// 各 provider 注册一个 fetcher：拿到当前凭证后按 Locator 去上游取字节。
type ArtifactFetcher interface {
    Fetch(ctx context.Context, rec ArtifactRecord) (body io.ReadCloser, mime string, err error)
}

type ArtifactStore struct { index *artifactIndex; cache *artifactCache; fetchers map[string]ArtifactFetcher; publicPath string }

func (s *ArtifactStore) Record(rec ArtifactRecord) error                       // 登记映射（幂等）
func (s *ArtifactStore) Put(id string, r io.Reader) error                      // 写缓存（tmp + rename）
func (s *ArtifactStore) Lookup(id string) (ArtifactRecord, bool)               // 查映射
func (s *ArtifactStore) Open(ctx context.Context, id string) (io.ReadCloser, ArtifactRecord, error)
        // cache 命中直接开；未命中则 fetchers[rec.Provider].Fetch → TeeReader 边流边写回 cache
func (s *ArtifactStore) PublicURL(base func(string) string, rec ArtifactRecord) string // base("/files/<id>.<ext>")
func (s *ArtifactStore) RegisterFetcher(provider string, f ArtifactFetcher)
func (s *ArtifactStore) StartCleanup(ctx context.Context)                       // 只清 cache
```

index：

- 存储：`ARTIFACT_INDEX_PATH`（默认 `<ARTIFACT_DIR>/index.jsonl`），追加写 JSONL；启动时全量读入内存 map，
  条目数超过阈值时后台压缩重写。每条几百字节，不设上限、不清理。
- 写入时机：生成侧拿到定位符就 `Record`，先于任何链接发出。
- 幂等：同 `ID` 重复 `Record` 只更新 `AuthID` / `Name`。

cache：

- 目录 `ARTIFACT_DIR`，文件 `<id>.<ext>`，先写 `.tmp` 再 `rename`，避免半文件被读到；正在回源的同一 `id` 用
  singleflight 合并，只打一次上游。
- 清理：`ARTIFACT_MAX_TOTAL_MB`（默认 2048）按总大小删最旧（atime/mtime）；`ARTIFACT_MAX_AGE_DAYS`
  （默认 0 不限）；后台每 10 分钟扫一次；日志打印删除数量。**删的只是字节，index 不动。**
- 启动时扫目录重建"已缓存"集合。

ID 规则：

- ChatGPT 图片：官网 `file_id`（`file_0000…`，全局唯一，去掉前缀外的非法字符）；
- ChatGPT 沙箱文件：`sha1(conv_id + msg_id + sandbox_path)` 前 32 位；
- Grok：资源 URL 中的 UUID 段，取不到则 `sha1(url)` 前 32 位。

路径安全：`id` 只允许 `[A-Za-z0-9_-]`，`ext` 白名单（png jpg jpeg webp gif mp4 webm pdf txt csv md json
docx xlsx pptx zip）。

凭证解析（回源时）：

- chatgpt-web：`ChatGPTAccounts` 按 `AuthID` 取 `coreauth.Auth`，用其当前 `access_token` 构造
  `sentinel.Client`（沿用 `PROXY_URL`）；凭证已删除或不可用时，**不**尝试其它账号（官网不允许跨账号取文件），
  直接 502 并记日志。
- grok-web：`GrokAccounts` 按 `AuthID` 取 `grok.Credential`，`grok.NewClient(cfg, cred)`（带 cookie / Statsig /
  代理）。同样不跨账号。

### 4.2 静态路由（`wukong/server/router.go`）

- 新增 `GET /files/:name`（`ARTIFACT_PUBLIC_PATH`，默认 `/files`）：解析 `<id>.<ext>`，校验后 `store.Open`：
  - cache 命中：`http.ServeContent`，`Content-Type`、`Content-Length`、
    `Cache-Control: public, max-age=31536000, immutable`，支持 `Range`（视频拖动）；
  - cache 未命中、index 命中：回源取回，边流边写；此时无法预知长度、不支持 Range，`Content-Type` 取上游值，
    响应头加 `X-Artifact-Source: upstream` 便于排查；下一次访问即命中缓存；
  - 回源失败：凭证缺失 / 上游 4xx → 502 并带简短原因；index 缺失 → 404。
  - 沙箱文件加 `Content-Disposition: inline; filename="<Name>"`。
- `/api/image/proxy`、`/api/pdf/proxy` 改为兼容别名：把 query 换算成 `id`，`index` 命中就走同一条 `Open` 链；
  `index` 没有（升级前发出的旧链接）才退回原来的会话逻辑，并在成功时顺手 `Record` + `Put` 回填。
- 现有 `r.Static("/images", cfg.ImageDir)` 保留不动。

### 4.3 ChatGPT 侧落盘（`wukong/server/engine.go`、`wukong/sentinel`）

- `sentinel.Client` 新增：
  - `DownloadFileByID(ctx, fileID, convID) (io.ReadCloser, mime string, err)`：复用 `resolveFileDownloadURL`；
  - `DownloadSandboxFile(ctx, convID, msgID, sandboxPath) (io.ReadCloser, mime, err)`：复用 `resolvePDFDownloadURL`。
  现有 `ProxyImageByFileID` / `ProxyPDFBySandboxPath` 改为基于这两个方法实现。
- `engine.go`：
  - 结果就位后（`Complete` 在拿到 `result` 之后、`Stream` 在 `artifact_slot_final` / 结束前），对
    `result.ImageFileIDs` 与 `sandboxFilesForHandler(result)` 逐个 `store.Record`（`AuthID` 来自本轮
    `ChatEnv` 对应的凭证，需要 executor 把 `auth.ID` 传进 `ChatEnv`）；
  - 图片随即用会话 client `DownloadFileByID` → `store.Put`（每张 1–3 MB，官网直链通常 < 2 s，4 张并行），
    这份字节同时供 4.5 内联使用；沙箱文件后台 `Put`，不阻塞；
  - `artifactMarkdown` 输出 `store.PublicURL(...)`；因为映射已登记，即使下载失败链接也照发，访问时回源；
  - 流式：`artifact` 事件里的 `url` 与末尾 markdown 一致，都是 `/files/<id>`。
- `handler_proxy.go`：`HandleImageProxy` / `HandlePDFProxy` 按 4.2 变成兼容别名。
- 注册 fetcher：`store.RegisterFetcher("chatgpt-web", chatgptArtifactFetcher{accounts})`，实现见 4.1 凭证解析。

### 4.4 Grok 侧落盘（`wukong/grok`、`wukong/cliproxy/grok_executor.go`）

- `grok.Config` 新增钩子 `RewriteAssetURL func(ctx context.Context, c *Client, rawURL string) string`，
  为空时行为不变。
- `grok.Client` 新增 `FetchAsset(ctx, rawURL) (io.ReadCloser, mime string, err)`：用客户端已有的带 cookie /
  Statsig 头的 http client 请求 `assets.grok.com` / `imagine-public.x.ai`（沿用 `trustedImageAssetHost` 白名单）。
- `engine.go` 里所有产生对外 URL 的点（`imageMarkdown`、`result.Images`、`VideoURL`、`result.Text` 中的
  视频 markdown、聊天里顺带出的图）在 `emit` 前经过钩子。
- 视频改用普通链接 `[Generated Video](…/files/<id>.mp4)`，不再用 `![]()`。
- `grok_executor.go`：`NewGrokExecutor(cfg, store)`，在 `cfg.RewriteAssetURL` 里：
  1. `store.Record(ArtifactRecord{Provider:"grok-web", AuthID: auth.ID, Locator{URL: raw}})`；
  2. 图片：同步 `FetchAsset → store.Put`（供内联）；视频：后台 goroutine 下载，超时
     `ARTIFACT_VIDEO_TIMEOUT_SEC`（默认 120）则放弃，等首次访问时再回源；
  3. 返回 `store.PublicURL(...)`。**任何情况下都不再返回 `assets.grok.com` 原链**；下载失败只记 warn，
     链接照发，访问时按映射回源。
- 注册 fetcher：`store.RegisterFetcher("grok-web", grokArtifactFetcher{accounts, cfg})`，用 `AuthID` 对应
  凭证的 `grok.Client.FetchAsset`。

### 4.5 图片内联 `images[]`（`wukong/server/openai_types.go`、`engine.go`、`wukong/grok/engine.go`）

- `Message` / `Delta` 新增 `Images []ImagePart json:"images,omitempty"`，
  `ImagePart{Type:"image_url", ImageURL:{URL:"data:<mime>;base64,<b64>"}, Index int}`，字段与
  `internal/translator/codex/openai/chat-completions/codex_openai_response.go` 输出一致。
- 非流式：`choices[0].message.images`；流式：与 `finish_reason:"stop"` 同一个 chunk 的 `delta.images`。
- 数据来源：生成时下载的那份字节（已写入 cache），不二次下载；若该图当轮下载失败则不内联，只给链接。
- 开关 `ARTIFACT_INLINE_IMAGES`（默认 `true`）；单张超过 `ARTIFACT_INLINE_MAX_MB`（默认 8）不内联。
- Grok 路径的 `OpenAIChunk` / `OpenAICompletion` 同步加字段。
- 已有 `artifact_delivery=base64` 的 `sentinel` 事件不受影响。

### 4.6 历史图片回挂（`wukong/server/handler_chat_input.go`、`engine.go`）

- 新增 `extractHistoryArtifacts(messages, isOwnURL func(string) (id string, ok bool)) []historyRef`：
  从最后一条 user 之前的 assistant 消息里提取 markdown 图片链接和 `images[]`（若客户端回传），
  只认指向本网关 `/files/<id>` 或 `/api/image/proxy?…file_id=` 的；按时间倒序取最近
  `ARTIFACT_HISTORY_REATTACH_MAX`（默认 4）张。
- `prepare`（无 `conversation_id` 分支）：把这些文件经 `store.Open` 读出（cache 命中直读，否则按映射回源），
  经 `UploadFile` 挂为本轮附件；
  `flattenHistory` 输出里把对应链接替换为 `[图片 N]`，并在 `[Current message]` 前加一行
  `[Attached images: 图片 1 = 上一轮生成的第 1 张 …]` 让模型建立对应关系。
- `uploadAttachments` 增加对 `store:<id>` 引用的处理（直接读磁盘，不走 http）。
- 仅在本轮是生图请求或历史中存在本网关图片时触发，纯文本对话零开销。

### 4.7 续接轮次只返回本轮新图（`engine.go`、`session.go`）

- `sessionEntry` 新增 `returnedFiles map[string]struct{}`。
- `artifactMarkdown` / `images[]` 组装前过滤已返回的 `file_id`；`sentinel` 事件保持完整（含 slot / revision），
  需要全量图槽状态的客户端从事件里拿。

### 4.8 配置（`wukong/server/config.go`，全部环境变量）

| 变量 | 默认 | 说明 |
|------|------|------|
| `ARTIFACT_DIR` | `artifacts` | 缓存目录（相对工作目录） |
| `ARTIFACT_INDEX_PATH` | `<ARTIFACT_DIR>/index.jsonl` | 映射表文件，持久、不清理 |
| `ARTIFACT_PUBLIC_PATH` | `/files` | 对外路径前缀 |
| `ARTIFACT_MAX_TOTAL_MB` | `2048` | 缓存总大小上限，超出删最旧（只删字节） |
| `ARTIFACT_MAX_AGE_DAYS` | `0` | 缓存按天清理，0 不限 |
| `ARTIFACT_FETCH_TIMEOUT_SEC` | `60` | 访问时回源单次超时 |
| `ARTIFACT_INLINE_IMAGES` | `true` | 图片是否内联 `images[]` |
| `ARTIFACT_INLINE_MAX_MB` | `8` | 单张内联上限 |
| `ARTIFACT_HISTORY_REATTACH_MAX` | `4` | 历史回挂张数 |
| `ARTIFACT_VIDEO_TIMEOUT_SEC` | `120` | Grok 视频后台预下载超时（超时不影响链接，访问时再回源） |
| `ARTIFACT_BASE_URL` | （已有） | 对外基址，链接前缀 |

`IMAGE_DIR` / `/images` 保留兼容，不再新用。

## 5. 分期

| 阶段 | 内容 | 解决 | 主要文件 |
|------|------|------|----------|
| **1** | 4.1 index + cache + fetcher 接口 + 4.2 路由（含回源）+ 4.3 ChatGPT 侧 + 4.4 Grok 侧 + 清理 + 4.8 配置 | P1 P2 P3 | `server/artifact_index.go`(新) `server/artifact_cache.go`(新) `server/artifact_store.go`(新) `server/artifact_fetchers.go`(新) `server/router.go` `server/engine.go` `server/handler_proxy.go` `server/config.go` `sentinel/image.go` `sentinel/pdf.go` `grok/engine.go` `grok/client.go`(新增方法) `grok/types.go` `cliproxy/executor.go`(ChatEnv 带 auth.ID) `cliproxy/grok_executor.go` `cliproxy/auth.go` `cliproxy/grok_auth.go`(按 AuthID 取凭证) `cliproxy/cmd/wukong-gateway/main.go` |
| **2** | 4.5 内联 + 4.6 回挂 + 4.7 只返新图 | P4 P5 | `server/openai_types.go` `server/engine.go` `server/handler_chat_input.go` `server/session.go` `grok/engine.go` |

阶段 1 是硬故障修复，阶段 2 是体验。两期都只动 `wukong/`。若需更快上线，阶段 1 可再拆出
"Grok 直链 → 落盘"单独先发（P1 影响的是当下所有 Grok 生图用户）。

## 6. 测试

单元测试（`go test ./wukong/...`）：

- `artifact_index_test.go`：Record 幂等；JSONL 追加与重启后全量恢复；压缩重写不丢条目。
- `artifact_cache_test.go`：Put tmp+rename 原子性；ID/ext 校验拒绝路径穿越；按总大小清理顺序；
  清理后 index 条目仍在。
- `artifact_store_test.go`：Open 三级链——cache 命中不调 fetcher；cache 缺失走 fetcher 且写回；
  并发同 id 只打一次上游（singleflight）；fetcher 报错返回可区分的 502/404 语义；凭证缺失不跨账号。
- `router` `/files`：Content-Type、immutable 头、Range（缓存命中）、回源时 `X-Artifact-Source`、非法名 404。
- `engine`：`artifactMarkdown` 走 `/files/<id>`；Record 成功但 Put 失败时链接照发；`images[]` 组装与开关；
  历史回挂的提取与占位替换；续接过滤已返回 file_id。
- `grok`：`RewriteAssetURL` 钩子在图片 / 视频 / 聊天顺带出图三处都生效；钩子为空行为不变；
  视频 markdown 由 `![]()` 变为 `[]()`；任何分支都不出现 `assets.grok.com`。
- `handler_proxy`：index 命中走 Open；index 缺失退回会话逻辑并回填 Record + Put。

线上验收（部署后，沿用此前实测脚本）：

1. `grok-web/grok-imagine-image-lite` 出图 → 匿名 `curl` 链接，期望 `200 image/jpeg`（现为 403）。
2. `chatgpt-web/gpt-5-5-thinking-extended` + `picture_v2` 要 4 张 → 4 个 `/files/*.png` 链接 200；
   宝塔重启 CLIProxy 后再取，仍 200。
2b. 回源：手工删掉 `artifacts/` 里其中一张的字节文件（保留 `index.jsonl`），再取该链接 → 200 且响应头带
   `X-Artifact-Source: upstream`；再取一次 → 无该头（已回缓存）。
3. 生成一个 txt/pdf（Code Interpreter）→ `/files/*.pdf` 200，`Content-Disposition` 带原文件名。
4. Open WebUI 风格（无 `conversation_id`、仅文本历史，历史含上一轮 `/files/*.png` 链接）
   "把刚才那只猫换成狗" → 新图延续原图风格；响应含 `images[]`。
5. 带 `conversation_id` 续接 → 响应只含本轮新图。
6. 回归：`/v1/models`、`chatgpt-web/gpt-5-5` 两轮、`grok-web/grok-chat-fast` 流式、Claude 协议 `/v1/messages`。
7. 磁盘：把 `ARTIFACT_MAX_TOTAL_MB` 临时设成 10，连续生成后确认最旧文件被删、日志有记录、
   `index.jsonl` 条目不减少，被删文件的链接仍能 200（回源）。
8. 出站代理：服务器若配置了 `PROXY_URL`，回源请求应经代理（看代理侧日志或抓包），客户端侧不需要任何代理。

## 7. 部署

- 宝塔 CLIProxy → 设置 → 环境变量：按需加 `ARTIFACT_DIR`、`ARTIFACT_MAX_TOTAL_MB`；
  `ARTIFACT_BASE_URL` 已设为 `http://23.142.200.35:8317`。
- 服务器预留磁盘：默认 2 GB 缓存约可存 800–1500 张图；视频按 20–60 MB/条估算。超出只是被清出缓存、
  访问时回源，不会失效。
- `index.jsonl` 要纳入备份：它丢了，未缓存的产物就无法回源。文件很小（每条几百字节）。
- 目录属主 `www`，网关进程需可写。
- 以后套域名 / HTTPS：只改 `ARTIFACT_BASE_URL`，store 内文件不动，历史链接需客户端侧替换主机名。
- 替换二进制流程同前：停 → 备份 → 上传 → 755 → 启动，`config.yaml` / `auths/` 不动。

## 8. 风险与决策点

| 风险 | 处理 |
|------|------|
| 视频下载阻塞响应 | 不阻塞：先登记映射、发链接，后台预下载；未预下完时首次访问回源边流边写 |
| 磁盘被打满 | 缓存总大小上限 + 最旧优先删除，只删字节；被删产物访问时自动回源；日志告警 |
| 缓存被清后回源频繁打上游 | 只有缺失时才回源，singleflight 合并并发；正常使用下热数据都在缓存里 |
| 账号失效 / 凭证被删 | 已缓存的产物照常访问；未缓存的回源 502 并注明凭证不可用；不跨账号尝试 |
| `index.jsonl` 损坏 | 逐行解析、跳过坏行并告警；已缓存文件不受影响 |
| `images[]` 让响应体膨胀 | 默认开但可关；单张上限；只在生图轮次出现 |
| 历史回挂每轮上传耗时 | 只取最近 4 张；只在生图相关轮次触发 |
| `/files` 免鉴权 | ID 不可枚举；与现有 proxy 同等风险；如需可加 `ARTIFACT_REQUIRE_KEY` 开关后续实现 |
| Grok cookie 失效导致下载 403 | 链接照发（映射已在），访问时回源失败给 502；cookie 由现有 Clearance 刷新机制恢复后自动可用 |
| 沙箱文件官网侧持久性 | 已核正（1.1）：网页端会持久保存，缓存 + 回源都可行；临时模式会话 30 天后清理，靠生成时预下载覆盖 |

## 9. 交付物

- 代码 + 单元测试（`go test ./wukong/...` 全绿）。
- `wukong/README.md` 增加"产物存储"一节与环境变量表。
- `wukong/cliproxy/config.example.yaml` 注释补充 `ARTIFACT_*` 说明。
- 重新交叉编译 `cli-proxy-api`（Linux amd64），附 SHA256。

## 10. 实施记录

### 阶段 1（提交 `d2122038`，2026-09-07 线上验收通过）

与计划的差异：

- 回源时**先完整写入缓存再回给客户端**，而不是边流边写。缓存文件因此一定完整，缓存命中路径可支持
  `Range`；代价是首次回源要等下载完成（图片数秒，视频数十秒）。
- 沙箱文件与图片的回源都复用 sentinel 已有的 `DownloadFileByFileID` / `DownloadSandboxFile`，没有新增
  `DownloadFileByID` 方法；Grok 复用已有的 `DownloadAsset`。
- Grok 钩子挂在 `grok.Client.SetAssetRewriter` 上（按凭证绑定），而不是 `grok.Config` 字段。
- 映射里没有凭证 ID 的旧记录（升级前由会话代理回填的）回源时会逐个尝试同 provider 的启用凭证；
  记了凭证 ID 的严格只用那一个。

线上验收（23.142.200.35:8317）：Grok 图匿名 200（原 403）；ChatGPT 4 图流式全部 `/files`、重启后仍 200；
删缓存文件后回源 200 且带 `X-Artifact-Source: upstream`，二次命中；沙箱 `notes.txt` 200 带
`Content-Disposition`；Grok 视频 `[Generated Video](…/files/<uuid>.mp4)` 200 `video/mp4`、`Range` 206；
回归全部通过。

### 阶段 2（2026-09-07 线上验收通过）

线上验收：Open WebUI 风格（无 `conversation_id`、只回传文本历史）"把刚才那只猫换成柯基"→ 新图与原图同背景、
同构图、同围巾，仅主体替换，证明历史图片确实被重新挂进了本轮；非流式 `message.images[]` 与流式收尾 chunk
的 `delta.images[]` 均为 data URL，字节数与 `/files` 链接取回一致，纯文本响应无 `images` 字段；Grok 非流式 /
流式同样带 `images[]`；带 `conversation_id` 续接"再来一张兔子"→ markdown 与 `images[]` 各 1 张、不含旧图；
回归通过。

- 4.5 `images[]`：`Message.Images` / `Delta.Images`，数据来自缓存（缺失则回源），`ARTIFACT_INLINE_IMAGES`
  / `ARTIFACT_INLINE_MAX_MB`；Grok 路径在 executor 侧把 `/files` 链接反查回 id 后同样内联。
- 4.6 历史回挂：`server/history_images.go`。识别历史 assistant 消息里的本网关链接（只看路径，换过域名
  的旧链接也认）与回传的 `images[]`（data URL）；文本占位 `[图片 N]`，未挂上的旧图占位 `[图片]`；
  附件引用用 `artifact:<id>` 走存储直读，`uploadAttachments` 也认客户端直接回传的 `/files` 链接。
- 4.7 续接差集：`sessionEntry.returned`，markdown 与 `images[]` 只含新图，`sentinel` 事件不变；
  续接轮次没有新图时不再贴任何图片链接。
