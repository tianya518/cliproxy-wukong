package server

// handler_chat.go —— ChatHandler 依赖持有者。
//
// 对话编排全在 engine.go 的 Engine 里。网关形态下 /v1/chat/completions 由 cliproxy
// 提供，wukong 侧不再挂 gin 聊天处理器；这里只保留 ChatHandler 与构造器，供产物
// 代理与账号管理路由复用（见 handler_proxy.go / RegisterArtifactAndAdminRoutes）。

// ChatHandler 持有依赖，供产物代理等非对话路由使用。
type ChatHandler struct {
	cfg     *ServerConfig
	engine  *Engine
	session *SessionManager
}

// NewChatHandler 创建 ChatHandler。
func NewChatHandler(cfg *ServerConfig, pool *TokenPool, session *SessionManager) *ChatHandler {
	return &ChatHandler{cfg: cfg, engine: NewEngine(cfg, pool, session), session: session}
}
