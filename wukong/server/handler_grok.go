package server

import (
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v7/wukong/grok"
)

// GrokHandler 管理 Grok Web SSO，落盘 grok.json。和 /chatgpt 一样不走调用方密码。
type GrokHandler struct {
	store *grok.AccountStore
}

func NewGrokHandler(store *grok.AccountStore) *GrokHandler {
	return &GrokHandler{store: store}
}

func (h *GrokHandler) HandleStatus(c *gin.Context) {
	accounts := []grok.AccountPublic{}
	if h.store != nil {
		accounts = h.store.PublicAccounts()
	}
	c.JSON(http.StatusOK, gin.H{
		"status":   "ok",
		"provider": "grok-web",
		"note":     NoteGrok,
		"total":    len(accounts),
		"accounts": accounts,
	})
}

func (h *GrokHandler) HandleUpload(c *gin.Context) {
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: ErrorDetail{Message: "read body failed", Type: "invalid_request_error"},
		})
		return
	}
	if strings.TrimSpace(string(raw)) == "" {
		_ = c.Request.ParseForm()
		raw = []byte(firstNonEmptyForm(c, "accounts", "sso", "sso_token", "text"))
	}
	added, err := h.store.ImportRaw(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: ErrorDetail{Message: err.Error(), Type: "invalid_request_error"},
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":   "success",
		"provider": "grok-web",
		"added":    added,
		"total":    h.store.Count(),
	})
}

func (h *GrokHandler) HandleAddSingle(c *gin.Context) {
	token := c.Param("token")
	if strings.TrimSpace(token) == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: ErrorDetail{Message: "sso token is required", Type: "invalid_request_error"},
		})
		return
	}
	added, err := h.store.ImportRaw([]byte(token))
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: ErrorDetail{Message: err.Error(), Type: "invalid_request_error"},
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":   "success",
		"provider": "grok-web",
		"added":    added,
		"total":    h.store.Count(),
	})
}

func (h *GrokHandler) HandleClear(c *gin.Context) {
	if err := h.store.Clear(); err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: ErrorDetail{Message: err.Error(), Type: "server_error"},
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":   "success",
		"provider": "grok-web",
		"total":    0,
	})
}

func (h *GrokHandler) HandleCheck(c *gin.Context) {
	results := h.store.CheckAll(c.Request.Context())
	valid := 0
	for _, result := range results {
		if result.Valid {
			valid++
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"status":   "success",
		"provider": "grok-web",
		"total":    len(results),
		"valid":    valid,
		"invalid":  len(results) - valid,
		"results":  results,
	})
}

func firstNonEmptyForm(c *gin.Context, keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(c.PostForm(key)); v != "" {
			return v
		}
	}
	return ""
}
