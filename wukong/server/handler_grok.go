package server

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v7/wukong/grok"
)

// GrokHandler 管理 Grok Web SSO。网关形态走 GrokAccountAdmin（auth-dir）；
// admin 为 nil 时仍写 grok.json（单测 / 无网关形态）。
type GrokHandler struct {
	store *grok.AccountStore
	admin GrokAccountAdmin
}

func NewGrokHandler(store *grok.AccountStore, admin GrokAccountAdmin) *GrokHandler {
	return &GrokHandler{store: store, admin: admin}
}

func (h *GrokHandler) HandleStatus(c *gin.Context) {
	accounts := []grok.AccountPublic{}
	if h.admin != nil {
		accounts = h.admin.PublicAccounts()
	} else if h.store != nil {
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
	added, err := h.importRaw(raw)
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
		"total":    h.count(),
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
	added, err := h.importRaw([]byte(token))
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
		"total":    h.count(),
	})
}

func (h *GrokHandler) HandleClear(c *gin.Context) {
	if err := h.clear(); err != nil {
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
	withQuota := boolQuery(c, "quota", true)
	results := h.checkAll(c.Request.Context(), withQuota)
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

// HandleQuota 只拉额度，不验会话。?id= 可指定单个账号（账号 ID / auth-dir 文件名），
// 面板按凭证文件刷新时用。/grok/check 会顺带带上同样的额度字段。
func (h *GrokHandler) HandleQuota(c *gin.Context) {
	id := strings.TrimSpace(c.Query("id"))
	results := h.quota(c.Request.Context(), id)
	if id != "" && len(results) == 0 {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: ErrorDetail{Message: "grok-web account not found: " + id, Type: "not_found"},
		})
		return
	}
	ok := 0
	for _, result := range results {
		if result.Error == "" {
			ok++
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"status":   "success",
		"provider": "grok-web",
		"total":    len(results),
		"ok":       ok,
		"failed":   len(results) - ok,
		"accounts": results,
	})
}

func (h *GrokHandler) count() int {
	if h.admin != nil {
		return h.admin.Count()
	}
	if h.store != nil {
		return h.store.Count()
	}
	return 0
}

func (h *GrokHandler) importRaw(raw []byte) (int, error) {
	if h.admin != nil {
		return h.admin.ImportRaw(raw)
	}
	return h.store.ImportRaw(raw)
}

func (h *GrokHandler) clear() error {
	if h.admin != nil {
		return h.admin.Clear()
	}
	return h.store.Clear()
}

func (h *GrokHandler) checkAll(ctx context.Context, withQuota bool) []grok.AccountCheckResult {
	if h.admin != nil {
		return h.admin.CheckAll(ctx, withQuota)
	}
	if h.store != nil {
		return h.store.CheckAll(ctx, withQuota)
	}
	return nil
}

func (h *GrokHandler) quota(ctx context.Context, id string) []grok.AccountQuotaResult {
	if h.admin != nil {
		return h.admin.Quota(ctx, id)
	}
	if h.store != nil {
		return h.store.Quota(ctx, id)
	}
	return nil
}

// boolQuery 解析 ?key=0/false/no 之类的开关，缺省返回 def。
func boolQuery(c *gin.Context, key string, def bool) bool {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return def
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return def
	}
	return value
}

func firstNonEmptyForm(c *gin.Context, keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(c.PostForm(key)); v != "" {
			return v
		}
	}
	return ""
}
