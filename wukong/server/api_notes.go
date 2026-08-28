package server

// 给打开 /、/chatgpt、/grok 的人看的路径说明，避免 /v1、/chatgpt、/tokens、/grok 互相搞混。
const (
	NoteV1 = "/v1 是 OpenAI 兼容对话口，给 Open WebUI 等客户端用；独立 API 服务这条只跑 ChatGPT，不是账号管理。"

	NoteChatGPT = "/chatgpt 管理 ChatGPT 网页凭证，落盘 chatgpt.json。/tokens 是旧名，与这里完全等价。Grok 请走 /grok。"

	NoteGrok = "/grok 管理 Grok.com 网页 SSO，落盘 grok.json。ChatGPT 请走 /chatgpt，不要把 JWT 贴到这里。"
)
