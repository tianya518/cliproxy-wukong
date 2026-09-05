package server

// 给打开 /、/chatgpt、/grok 的人看的路径说明，避免 /v1、/chatgpt、/tokens、/grok 互相搞混。
const (
	NoteV1 = "/v1 是 OpenAI 兼容对话口，给 Open WebUI 等客户端用；独立 API 服务这条只跑 ChatGPT，不是账号管理。"

	NoteChatGPT = "/chatgpt 管理 ChatGPT 网页凭证，落盘 auth-dir/chatgpt-web-*.json。灌号 POST /chatgpt/upload {\"tokens\":\"...\"}，接受 chatgpt.com/api/auth/session 整段 JSON、<access>----<session>、rt:<refresh>、裸 access/session token。模型目录 401 会写到凭证 status_message，不再只打日志。/tokens 是旧名。Grok 请走 /grok。"

	NoteGrok = "/grok 管理 Grok.com 网页 SSO，落盘 auth-dir/grok-web-*.json。额度看 /grok/quota：windows 是 2 小时滚动窗口，billing 是订阅的周/月已用百分比与重置券；/grok/check 顺带带上（想只验会话用 /grok/check?quota=0）。ChatGPT 请走 /chatgpt，不要把 JWT 贴到这里。"
)
