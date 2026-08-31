package server

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v7/wukong/grok"
)

// ChatGPTAccountAdmin writes /chatgpt imports into cliproxy auth-dir
// and answers status / check from Manager. When nil, handlers still
// use TokenPool only (tests / non-gateway mode).
type ChatGPTAccountAdmin interface {
	Import(chunks []string) (int, error)
	Clear() error
	Stats() (total, valid, errored int)
	ErrorIDs() []string
	CheckAll() []TokenCheckResult
}

// GrokAccountAdmin writes /grok imports into cliproxy auth-dir.
// When nil, handlers still use AccountStore (tests / non-gateway mode).
type GrokAccountAdmin interface {
	ImportRaw([]byte) (int, error)
	Clear() error
	Count() int
	PublicAccounts() []grok.AccountPublic
	CheckAll(ctx context.Context) []grok.AccountCheckResult
}

// RegisterArtifactAndAdminRoutes 把产物代理与账号管理路由挂到给定路由器上。
//
// wukong-gateway 单一入口形态用它，把这批路由直接挂到 cliproxy 网关的 gin 引擎上。
// 只注册 cliproxy 网关不提供、且与其现有路由不冲突的路径：产物代理 /api/*、静态
// 图片 /images、ChatGPT 账号 /chatgpt|/tokens、Grok 账号 /grok。刻意不含 / 与 /v1/*
// ——这些由 cliproxy 拥有。这些路由注册在网关根引擎、不进 api-key 鉴权组（图片链接
// 要能被末端客户端直接取；账号管理接口本就无鉴权，注意别暴露到公网）。
func RegisterArtifactAndAdminRoutes(r gin.IRouter, cfg *ServerConfig, pool *TokenPool, session *SessionManager, grokStore *grok.AccountStore, chatgptAdmin ChatGPTAccountAdmin, grokAdmin GrokAccountAdmin) {
	// ChatGPT 网页凭证。正式路径是 /chatgpt；/tokens 是旧名，两边同一套处理函数。
	tokens := NewTokensHandler(pool, session, chatgptAdmin)
	for _, prefix := range []string{"/chatgpt", "/tokens"} {
		r.GET(prefix, tokens.HandleStatus)
		r.POST(prefix+"/upload", tokens.HandleUpload)
		r.POST(prefix+"/clear", tokens.HandleClear)
		r.GET(prefix+"/add/:token", tokens.HandleAddSingle)
		r.GET(prefix+"/errors", tokens.HandleErrors)
		r.GET(prefix+"/check", tokens.HandleCheck)
	}

	// Grok Web SSO
	grokH := NewGrokHandler(grokStore, grokAdmin)
	r.GET("/grok", grokH.HandleStatus)
	r.POST("/grok/upload", grokH.HandleUpload)
	r.POST("/grok/clear", grokH.HandleClear)
	r.GET("/grok/add/:token", grokH.HandleAddSingle)
	r.GET("/grok/check", grokH.HandleCheck)

	// 产物代理与静态图片
	chat := NewChatHandler(cfg, pool, session)
	r.GET("/api/image/proxy", chat.HandleImageProxy)
	r.GET("/api/pdf/proxy", chat.HandlePDFProxy)
	r.Static("/images", cfg.ImageDir)
}
