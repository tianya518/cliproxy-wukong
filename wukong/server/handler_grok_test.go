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

func TestGrokUploadAndClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grok.json")
	store := grok.NewAccountStore(path, grok.Config{})
	cfg := &ServerConfig{GrokFile: path, ImageDir: t.TempDir()}
	pool := NewTokenPool(filepath.Join(t.TempDir(), "chatgpt.json"), 0)
	session := NewSessionManager(cfg)
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	RegisterArtifactAndAdminRoutes(r, cfg, pool, session, store, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/grok/upload", bytes.NewBufferString(`{"sso":"abc","name":"plus-1"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload status=%d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/grok", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	var got struct {
		Total    int                  `json:"total"`
		Accounts []grok.AccountPublic `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Total != 1 || got.Accounts[0].Name != "plus-1" || !got.Accounts[0].HasSSO || got.Accounts[0].ID == "" {
		t.Fatalf("%#v", got)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("abc")) {
		t.Fatal("status leaked sso token")
	}

	req = httptest.NewRequest(http.MethodPost, "/grok/clear", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("clear %d %s", w.Code, w.Body.String())
	}
	if store.Count() != 0 {
		t.Fatalf("count=%d", store.Count())
	}
}
