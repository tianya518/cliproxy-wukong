package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

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

// fakeGrokAdmin 只验路由接线，不打上游。
type fakeGrokAdmin struct {
	quota      []grok.AccountQuotaResult
	check      []grok.AccountCheckResult
	lastQuotaF *bool
}

func (f *fakeGrokAdmin) ImportRaw([]byte) (int, error)        { return 0, nil }
func (f *fakeGrokAdmin) Clear() error                         { return nil }
func (f *fakeGrokAdmin) Count() int                           { return len(f.quota) }
func (f *fakeGrokAdmin) PublicAccounts() []grok.AccountPublic { return nil }

func (f *fakeGrokAdmin) Quota(_ context.Context, id string) []grok.AccountQuotaResult {
	if id == "" {
		return f.quota
	}
	for _, result := range f.quota {
		if result.ID == id {
			return []grok.AccountQuotaResult{result}
		}
	}
	return nil
}

func (f *fakeGrokAdmin) CheckAll(_ context.Context, withQuota bool) []grok.AccountCheckResult {
	f.lastQuotaF = &withQuota
	return f.check
}

func TestGrokQuotaRouteAndCheckQuotaToggle(t *testing.T) {
	synced := time.Unix(1700000000, 0).UTC()
	admin := &fakeGrokAdmin{
		quota: []grok.AccountQuotaResult{
			{ID: "a", Name: "plus-1", Tier: grok.TierAuto, SyncedAt: &synced, Windows: []grok.QuotaWindow{
				{Mode: "auto", Remaining: 11, Total: 20, WindowSeconds: 7200},
			}},
			{ID: "b", Error: "boom"},
		},
		check: []grok.AccountCheckResult{
			{ID: "a", Valid: true, Windows: []grok.QuotaWindow{{Mode: "auto", Remaining: 11, Total: 20}}},
		},
	}
	cfg := &ServerConfig{ImageDir: t.TempDir()}
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	RegisterArtifactAndAdminRoutes(r, cfg, nil, NewSessionManager(cfg), nil, nil, admin)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/grok/quota", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("quota status=%d body=%s", w.Code, w.Body.String())
	}
	var quota struct {
		Total    int                       `json:"total"`
		OK       int                       `json:"ok"`
		Failed   int                       `json:"failed"`
		Accounts []grok.AccountQuotaResult `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &quota); err != nil {
		t.Fatal(err)
	}
	if quota.Total != 2 || quota.OK != 1 || quota.Failed != 1 {
		t.Fatalf("counts = %#v", quota)
	}
	if len(quota.Accounts[0].Windows) != 1 || quota.Accounts[0].Windows[0].Remaining != 11 {
		t.Fatalf("windows = %#v", quota.Accounts[0])
	}

	// ?id= 只取一个账号；没命中返回 404。
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/grok/quota?id=b", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("quota?id=b status=%d body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &quota); err != nil {
		t.Fatal(err)
	}
	if quota.Total != 1 || quota.Failed != 1 || quota.Accounts[0].ID != "b" {
		t.Fatalf("filtered = %#v", quota)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/grok/quota?id=missing", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("quota?id=missing status=%d body=%s", w.Code, w.Body.String())
	}

	// /grok/check 默认带额度，?quota=0 只验会话。
	for query, want := range map[string]bool{"": true, "?quota=0": false, "?quota=true": true} {
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/grok/check"+query, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("check%s status=%d", query, w.Code)
		}
		if admin.lastQuotaF == nil || *admin.lastQuotaF != want {
			t.Fatalf("check%s withQuota=%v want %v", query, admin.lastQuotaF, want)
		}
	}
}
