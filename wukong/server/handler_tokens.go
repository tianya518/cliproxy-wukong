package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// TokensHandler ChatGPT 网页凭证管理（/chatgpt，旧名 /tokens）。Grok 走 /grok。
type TokensHandler struct {
	pool    *TokenPool
	session *SessionManager
	admin   ChatGPTAccountAdmin
}

// NewTokensHandler 创建 TokensHandler。admin 为 nil 时只写 TokenPool。
func NewTokensHandler(pool *TokenPool, session *SessionManager, admin ChatGPTAccountAdmin) *TokensHandler {
	return &TokensHandler{pool: pool, session: session, admin: admin}
}

func (h *TokensHandler) importChunks(chunks ...string) (int, error) {
	if h.admin != nil {
		return h.admin.Import(chunks)
	}
	return h.pool.Add(chunks...), nil
}

func (h *TokensHandler) stats() (total, valid, errored int) {
	if h.admin != nil {
		return h.admin.Stats()
	}
	return h.pool.Stats()
}

// HandleStatus 查看 ChatGPT 凭证池 GET /chatgpt（旧名 /tokens）
func (h *TokensHandler) HandleStatus(c *gin.Context) {
	total, valid, errored := h.stats()
	c.JSON(http.StatusOK, gin.H{
		"status":          "ok",
		"provider":        "chatgpt-web",
		"note":            NoteChatGPT,
		"aliases":         []string{"/tokens"},
		"total":           total,
		"valid":           valid,
		"error":           errored,
		"active_sessions": h.session.Count(),
	})
}

// HandleUpload 上传 ChatGPT 凭证 POST /chatgpt/upload（旧名 /tokens/upload）
// Body: {"tokens": "token1\ntoken2\ntoken3"}
// 或 form: text=token1\ntoken2
func (h *TokensHandler) HandleUpload(c *gin.Context) {
	var body struct {
		Tokens string `json:"tokens" form:"text"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		_ = c.ShouldBind(&body)
	}

	if body.Tokens == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: ErrorDetail{Message: "tokens field is required", Type: "invalid_request_error"},
		})
		return
	}

	added, err := h.importChunks(splitUploadText(body.Tokens)...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: ErrorDetail{Message: err.Error(), Type: "server_error"},
		})
		return
	}

	total, valid, _ := h.stats()
	c.JSON(http.StatusOK, gin.H{
		"status":       "success",
		"provider":     "chatgpt-web",
		"added":        added,
		"tokens_count": valid,
		"total":        total,
	})
}

// HandleAddSingle 添加单个凭证 GET /chatgpt/add/:token（旧名 /tokens/add/:token）
func (h *TokensHandler) HandleAddSingle(c *gin.Context) {
	token := c.Param("token")
	if token == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: ErrorDetail{Message: "token is required", Type: "invalid_request_error"},
		})
		return
	}

	added, err := h.importChunks(token)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: ErrorDetail{Message: err.Error(), Type: "server_error"},
		})
		return
	}
	total, valid, _ := h.stats()
	c.JSON(http.StatusOK, gin.H{
		"status":       "success",
		"provider":     "chatgpt-web",
		"added":        added,
		"tokens_count": valid,
		"total":        total,
	})
}

// HandleClear 清空 ChatGPT 凭证 POST /chatgpt/clear（旧名 /tokens/clear）
func (h *TokensHandler) HandleClear(c *gin.Context) {
	if h.admin != nil {
		if err := h.admin.Clear(); err != nil {
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Error: ErrorDetail{Message: err.Error(), Type: "server_error"},
			})
			return
		}
	} else {
		h.pool.Clear()
	}
	c.JSON(http.StatusOK, gin.H{
		"status":       "success",
		"provider":     "chatgpt-web",
		"tokens_count": 0,
	})
}

// HandleErrors 查看失效凭证 GET /chatgpt/errors（旧名 /tokens/errors）
func (h *TokensHandler) HandleErrors(c *gin.Context) {
	var ids []string
	if h.admin != nil {
		ids = h.admin.ErrorIDs()
	} else {
		ids = h.pool.ErrorTokens()
	}
	c.JSON(http.StatusOK, gin.H{
		"status":       "success",
		"provider":     "chatgpt-web",
		"error_tokens": ids,
		"count":        len(ids),
	})
}

// HandleCheck 主动检测每条 ChatGPT 凭证 GET /chatgpt/check（旧名 /tokens/check）
// 含 Session 的会尝试换取 Access Token，仅 Access 的会探测存活；
// 结果同步更新失效标记与刷新后的 Access Token。
func (h *TokensHandler) HandleCheck(c *gin.Context) {
	var results []TokenCheckResult
	if h.admin != nil {
		results = h.admin.CheckAll()
	} else {
		results = h.pool.CheckAll()
	}
	validCount := 0
	for _, r := range results {
		if r.Valid {
			validCount++
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"status":   "success",
		"provider": "chatgpt-web",
		"total":    len(results),
		"valid":    validCount,
		"invalid":  len(results) - validCount,
		"results":  results,
	})
}
