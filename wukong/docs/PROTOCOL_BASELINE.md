# ChatGPT 官网接口基线（MCP 实抓）

> 最近一次抓取：2026-08-26，Chrome 151，账号 Plus，首页「聊天」标签。
> 上次基线：2026-07-07，Chrome 149。
> 目的：核对官网当前接口/字段/流程是否与 wukong 代码一致，作为"同步流程"依据。
> 约定：**协议头 / 风控 / PoW / Turnstile / UA 一律保持代码现状不动**，本文档仅记录事实。

---

## 场景一：纯文本对话（模型 gpt-5-6-thinking，「聊天」标签）

### 请求时序（fetch，`/backend-api/` 过滤）

1. `POST /backend-api/f/conversation/prepare`  → 返回 conduit_token（官网仍空 body）
2. `POST /backend-api/sentinel/chat-requirements/prepare` → 返回 prepare_token + turnstile
3. `POST /backend-api/sentinel/ping`（心跳，`{"status":"OK"}`）
4. `POST /backend-api/sentinel/chat-requirements/finalize` → 返回 sentinel token
5. `POST /backend-api/f/conversation`（主对话，SSE 流）
6. `GET /backend-api/conversation/{id}/stream_status` → `{"status":"IS_STREAMING"}`
7. 侧栏/遥测：`conversations`、`gizmos/{id}/conversations`、`textdocs`、`lat/r`、`sentinel/ping`
8. 风控旁路（代码不跟）：`GET /backend-api/sentinel/sdk.js`、`GET /backend-api/sentinel/frame.html`、`POST /backend-api/sentinel/req`

主链路 endpoint **没换**：还是 prepare → chat-requirements → `/f/conversation`。
`conversation_mode.kind` 仍是 `primary_assistant`。

### 关键请求：`POST /backend-api/f/conversation` body（2026-08-26 实抓，已脱敏）

```json
{
  "action": "next",
  "messages": [{
    "id": "<uuid>",
    "author": {"role": "user"},
    "create_time": 1787715054.15,
    "content": {"content_type": "text", "parts": ["只回复一个词：PING"]},
    "metadata": {
      "selected_sources": [],
      "serialization_metadata": {"custom_symbol_offsets": []}
    }
  }],
  "parent_message_id": "client-created-root",
  "model": "gpt-5-6-thinking",
  "client_prepare_state": "success",
  "timezone_offset_min": -480,
  "timezone": "Asia/Shanghai",
  "conversation_mode": {"kind": "primary_assistant"},
  "enable_message_followups": true,
  "system_hints": [],
  "supports_buffering": true,
  "supported_encodings": ["v1"],
  "client_contextual_info": {
    "is_dark_mode": false, "time_since_loaded": 59,
    "page_height": 909, "page_width": 1424, "pixel_ratio": 1,
    "screen_height": 1080, "screen_width": 1920,
    "app_name": "chatgpt.com",
    "has_web_push_capabilities": true,
    "web_push_notification_permission": "default"
  },
  "paragen_cot_summary_display_override": "allow",
  "force_parallel_switch": "auto",
  "thinking_effort": "extended",
  "local_function_names": ["local.continue_in_work"],
  "model_response_contracts": [{
    "id": "photo_upload_action.v1",
    "presets": ["cap:image", "cap:file", "placement:end"],
    "protocol_version": 1
  }]
}
```

### 相对 2026-07 基线 / 旧代码的差异（已同步进 `buildConversationBody`）

| 字段 | 2026-07 官网 | 2026-08-26 官网 | 代码 |
| --- | --- | --- | --- |
| `client_prepare_state` | `"none"` | `"success"`（prepare 成功后） | 现为 `"success"` |
| `local_function_names` | 无 | `["local.continue_in_work"]` | 已补。只是「继续到工作」UI 钩子 |
| `model_response_contracts` | 无 | `photo_upload_action.v1` | 已补。声明客户端能接图片/文件结构化回复 |
| `client_contextual_info` web push | 官网有、代码缺 | 仍有 | 已补 |
| 用户 `metadata` | 只有 sources + offsets | 同左 | 去掉了官网已不发的 github / developer_mode 空字段 |
| `history_and_training_disabled` | 非临时对话不发 | 同左 | 仅 `TempMode` 时写入 |
| endpoint / `conversation_mode` | `/f/conversation` + `primary_assistant` | 未变 | 未变 |

### 观察到但按约定不动的部分

- 官网 `oai-client-version=prod-36a6f6fd65c2f9e5ece552f6eefd7fd0ed94d10a`，`oai-client-build-number=9857890`；代码仍用旧 build hash / 6128297。
- 官网 UA 是 Chrome 151；代码浏览器身份仍按 Edge 146 常量。
- 官网 prepare **仍空 body**；代码继续带 `partial_query`。
- 新接口 `POST /backend-api/sentinel/req`（`flow=conversation` + `p=` 指纹串）和 `sentinel/frame.html` 属风控，不跟。

---

## 认证三步（endpoint 未变，内部有变化 —— 按约定不改）

### Step 1 `POST /backend-api/f/conversation/prepare`

- 请求 body：空（官网未带 body）。代码当前带 `partial_query`（前 5 字符），差异记录，不动。
- 响应：`{"status":"ok","conduit_token":"<JWT>"}`
  - JWT payload 含 `conduit_uuid` / `conduit_location` / `cluster` / `iat` / `exp` / `turn_topic_id`

### Step 2 `POST /backend-api/sentinel/chat-requirements/prepare`

- 响应：`{"persona":"chatgpt-paid","prepare_token":"gAAAAAB...","turnstile":{"required":true,"dx":"<大段数据>"}}`
- **重要变化：响应中已无 `proofofwork` 字段**（代码 sentinel/auth.go 仍解析 `proofofwork.seed/difficulty/required`）。
  - 即官网此接口当前不下发 PoW 挑战，只给 `prepare_token` + `turnstile`。
- `turnstile.required = true`，但当前代码不处理 turnstile 仍可正常对话 → 视为软要求 / 不阻断。**按约定不改。**

### Step 3 `POST /backend-api/sentinel/chat-requirements/finalize`

- 响应：`{"persona":"chatgpt-paid","token":"gAAAAAB...","expire_after":540,"expire_at":1783425978}`
- **`expire_after: 540`（秒）+ `expire_at`（unix 秒）**：可作为 sentinel token 缓存有效期依据（代码目前每轮重新 prepare+finalize）。

### 新接口 `POST /backend-api/sentinel/ping`

- 心跳，POST，无 body、无响应体（推测 204）。代码中无对应逻辑，不影响核心流程。

---

## 聊天 / 工作两个标签（2026-07 起）

官网首页顶部的「聊天」和「工作」是**两个产品面**，不是 `conversation_mode.kind` 的新取值。

| | 聊天（本项目对齐） | 工作（不对齐） |
| --- | --- | --- |
| 前端内部名 | `chatgpt` | `work_tab` / TPP |
| `conversation_mode.kind` | `primary_assistant` | 前端枚举里**没有** `work_mode` |
| 模型 | `gpt-5-6-thinking` 等，`is_work_mode_model=false` | `gpt-5.6-sol-wm` 等，`is_work_mode_model=true` |
| 额度 | ChatGPT 聊天额度 | 与 Codex / agent 共用的独立额度 |
| 形态 | 一问一答 | 多步 agent，产出文档/表格等成品 |

实测：用现有 `/f/conversation` + `primary_assistant` 打 `-wm` 模型也能回文本，
但那只是模型 slug 能通，并不等于实现了工作标签的 agent 运行时。
本项目把 `-wm` 模型从 `/v1/models` 默认列表里拿掉，显式指定 slug 仍可解析。

## 2026-08-26 补抓：生图 / 上传 / 下载（Chat 标签）

主对话体与纯文本相同：`gpt-5-6-thinking` + `thinking_effort=extended` + `client_prepare_state=success`，
**没有**单独的 `picture_v2` system_hint——模型自己决定生图。

| 场景 | 结果 |
| --- | --- |
| 文生图（1 张） | `POST /f/conversation` 后走 `GET /files/download/{file_id}`。query：`inline=false&download_intent=false&include_library_file_state=true&conversation_id=` |
| 一次命令 4 张独立图 | 请求体与单张相同。约 33s 出齐 4 个 `sediment://file_...`。`async_status=4`。工具名仍是 `t2uay3k.sj1i4kz`。**这次没有 `batch_requests` / `assistant/code` 节点**，张数以 mapping 里去重后的 file_id 为准。收图轮询仍适用（期望张数未知时等数量稳定） |
| 多类型上传 txt/csv/pdf/png | 仍是每个文件一次 `POST /backend-api/files`，再 `PUT oaiusercontent.com/files/{uuid}/raw`（201）。确认端点 `/uploaded` / `process_upload_stream` 这次窗口里没抓到，旧三步继续用 |
| 生成 hello.txt + hello.pdf 并下载 | 点击「下载文件」走 `GET /conversation/{id}/interpreter/download?message_id=...&sandbox_path=/mnt/data/hello.txt\|hello.pdf`。两种扩展名同一 `message_id`，与 `pdf.go` 一致 |

未复测：图生图（参考图改图）。7 月那份 `IMAGE_FLOW_CAPTURE.md` 仍作参考。

---

## 2026-08-26 补抓：ChatGPT 项目

项目是一类 gizmo（id `g-p-…`，前端叫 snorlax），不是 Work 标签。

| 动作 | 官网接口 | 响应要点 |
| --- | --- | --- |
| 列项目 | `GET /backend-api/gizmos/snorlax/sidebar?conversations_per_gizmo=0&owned_only=true` | `{items:[{gizmo:{gizmo:{id,display,instructions,…}}}], cursor}`，有 cursor 就翻页 |
| 创建 | `POST /backend-api/projects` body `{"name","instructions"}` | `{resource:{gizmo:{id:"g-p-…",…}}, error:null}` |
| 详情 | `GET /backend-api/gizmos/{g-p-id}` | `{gizmo:{…}, tools, files, …}` |
| 项目内会话 | `GET /backend-api/gizmos/{g-p-id}/conversations` | `{items, cursor}`，item 上 `gizmo_id` 与 `conversation_template_id` 都等于项目 id |
| 项目内开聊 | 与普通 Chat 同一套 `POST /backend-api/f/conversation` | **只改** `conversation_mode`：`{"kind":"gizmo_interaction","gizmo_id":"g-p-…"}`。顶层没有额外的 `gizmo_id` / `workspace_id` |

项目页 URL：`https://chatgpt.com/g/{g-p-id}/project`。
SSE `server_ste_metadata.turn_mode` 为 `"projects"` 可确认挂进了项目。
代码入口：`sentinel/project.go`、对话体 `buildConversationBody`、HTTP `/v1/projects`。
