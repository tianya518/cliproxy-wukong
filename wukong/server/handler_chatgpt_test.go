package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v7/wukong/grok"
)

func TestChatGPTRoutesAndTokensAlias(t *testing.T) {
	cfg := &ServerConfig{ChatGPTFile: filepath.Join(t.TempDir(), "chatgpt.json"), ImageDir: t.TempDir()}
	pool := NewTokenPool(cfg.ChatGPTFile, 0)
	session := NewSessionManager(cfg)
	store := grok.NewAccountStore(filepath.Join(t.TempDir(), "grok.json"), grok.Config{})
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	RegisterArtifactAndAdminRoutes(r, cfg, pool, session, store, nil, nil)

	const jwt = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJhIn0.sigA"
	req := httptest.NewRequest(http.MethodPost, "/chatgpt/upload", bytes.NewBufferString(`{"tokens":"`+jwt+`"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload %d %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/chatgpt", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var got struct {
		Provider string `json:"provider"`
		Total    int    `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Provider != "chatgpt-web" || got.Total != 1 {
		t.Fatalf("%s %#v", w.Body.String(), got)
	}

	req = httptest.NewRequest(http.MethodGet, "/tokens", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("legacy /tokens %d %s", w.Code, w.Body.String())
	}
}
