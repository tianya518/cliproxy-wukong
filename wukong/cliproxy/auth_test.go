package cliproxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestRegisterAuthsFromChatGPTFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chatgpt.json")
	exp := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	raw, err := json.Marshal(map[string]any{
		"version": 1,
		"tokens": []map[string]any{{
			"id":            "acct-1",
			"access_token":  jwtA,
			"refresh_token": "rt-live",
			"session_token": "st-live",
			"expires_at":    exp.Format(time.RFC3339),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := coreauth.NewManager(nil, nil, nil)
	n, err := RegisterAuthsFromChatGPTFile(context.Background(), mgr, path, "")
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	var got *coreauth.Auth
	for _, auth := range mgr.List() {
		if auth.Provider == ProviderKey {
			got = auth
			break
		}
	}
	if got == nil {
		t.Fatal("chatgpt-web auth not registered")
	}
	if got.ID != ProviderKey+"-acct-1" {
		t.Fatalf("id = %q", got.ID)
	}
	if got.Metadata["access_token"] != jwtA || got.Metadata["refresh_token"] != "rt-live" {
		t.Fatalf("metadata = %#v", got.Metadata)
	}
	if got.Metadata["session_token"] != "st-live" {
		t.Fatalf("session_token = %v", got.Metadata["session_token"])
	}
	if got.Attributes[coreauth.AttributeAuthKind] != coreauth.AuthKindOAuth {
		t.Fatalf("auth_kind = %q", got.Attributes[coreauth.AttributeAuthKind])
	}
	if token, err := accessTokenFrom(got); err != nil || token != jwtA {
		t.Fatalf("accessTokenFrom = %q %v", token, err)
	}

	n, err = RegisterAuthsFromChatGPTFile(context.Background(), mgr, filepath.Join(dir, "missing.json"), "")
	if err != nil || n != 0 {
		t.Fatalf("missing file should be 0, nil; n=%d err=%v", n, err)
	}
}

func TestRegisterAuthsFromChatGPTFileRefreshOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chatgpt.json")
	raw, err := json.Marshal(map[string]any{
		"version": 1,
		"tokens": []map[string]any{{
			"id":            "rt-only",
			"refresh_token": "rt-only-value",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	mgr := coreauth.NewManager(nil, nil, nil)
	n, err := RegisterAuthsFromChatGPTFile(context.Background(), mgr, path, "")
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	auth := mgr.List()[0]
	if auth.Metadata["refresh_token"] != "rt-only-value" {
		t.Fatalf("metadata = %#v", auth.Metadata)
	}
	if _, err = accessTokenFrom(auth); err == nil {
		t.Fatal("RT-only auth should not yield an access token yet")
	}
}

func TestRegisterAuthsPrefersPersistedAuthDir(t *testing.T) {
	dir := t.TempDir()
	chatgptPath := filepath.Join(dir, "chatgpt.json")
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	seed, err := json.Marshal(map[string]any{
		"version": 1,
		"tokens": []map[string]any{{
			"id":            "acct-1",
			"access_token":  jwtA,
			"refresh_token": "rt-old",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(chatgptPath, seed, 0o644); err != nil {
		t.Fatal(err)
	}
	persisted, err := json.Marshal(map[string]any{
		"type":          ProviderKey,
		"access_token":  jwtB,
		"refresh_token": "rt-new",
		"expired":       time.Date(2032, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(authDir, chatgptWebAuthFileName("acct-1")), persisted, 0o600); err != nil {
		t.Fatal(err)
	}

	mgr := coreauth.NewManager(nil, nil, nil)
	n, err := RegisterAuthsFromChatGPTFile(context.Background(), mgr, chatgptPath, authDir)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got := mgr.List()[0]
	if got.ID != chatgptWebAuthID(chatgptWebAuthFileName("acct-1")) {
		t.Fatalf("id = %q", got.ID)
	}
	if got.Metadata["access_token"] != jwtB || got.Metadata["refresh_token"] != "rt-new" {
		t.Fatalf("should load refreshed auth-dir file, got %#v", got.Metadata)
	}
}

func TestRefreshPersistsToAuthDir(t *testing.T) {
	dir := t.TempDir()
	chatgptPath := filepath.Join(dir, "chatgpt.json")
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	seed, err := json.Marshal(map[string]any{
		"version": 1,
		"tokens": []map[string]any{{
			"id":            "acct-1",
			"access_token":  jwtA,
			"refresh_token": "rt-old",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(chatgptPath, seed, 0o644); err != nil {
		t.Fatal(err)
	}

	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	mgr := coreauth.NewManager(store, nil, nil)
	if _, err = RegisterAuthsFromChatGPTFile(context.Background(), mgr, chatgptPath, authDir); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(authDir, chatgptWebAuthFileName("acct-1"))
	migrated, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("migrate should write auth-dir: %v", err)
	}
	var migratedMeta map[string]any
	if err = json.Unmarshal(migrated, &migratedMeta); err != nil {
		t.Fatal(err)
	}
	if migratedMeta["type"] != ProviderKey || migratedMeta["access_token"] != jwtA || migratedMeta["refresh_token"] != "rt-old" {
		t.Fatalf("migrated %#v", migratedMeta)
	}

	exp := time.Date(2033, 4, 5, 6, 7, 8, 0, time.UTC)
	exec := NewExecutor(nil, "")
	exec.refreshFromRefreshToken = func(string, string, string) (string, string, time.Time, error) {
		return jwtC, "rt-refreshed", exp, nil
	}
	auth := mgr.List()[0]
	refreshed, err := exec.Refresh(context.Background(), auth.Clone())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = mgr.Update(context.Background(), refreshed); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err = json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["type"] != ProviderKey || meta["access_token"] != jwtC || meta["refresh_token"] != "rt-refreshed" {
		t.Fatalf("persisted %#v", meta)
	}
	if meta["expired"] != exp.Format(time.RFC3339) {
		t.Fatalf("expired = %v", meta["expired"])
	}
}

func TestChatGPTAccountsImportWritesAuthDir(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	mgr := coreauth.NewManager(store, nil, nil)
	accounts := NewChatGPTAccounts(mgr, authDir, nil)

	added, err := accounts.Import([]string{jwtA})
	if err != nil || added != 1 {
		t.Fatalf("import added=%d err=%v", added, err)
	}
	if got := len(mgr.List()); got != 1 {
		t.Fatalf("manager auths = %d", got)
	}
	auth := mgr.List()[0]
	if auth.Provider != ProviderKey {
		t.Fatalf("provider = %q", auth.Provider)
	}
	if auth.Attributes[coreauth.AttributePath] == "" {
		t.Fatal("missing auth-dir path")
	}
	raw, err := os.ReadFile(auth.Attributes[coreauth.AttributePath])
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err = json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["type"] != ProviderKey || meta["access_token"] != jwtA {
		t.Fatalf("persisted %#v", meta)
	}
	if total, valid, errored := accounts.Stats(); total != 1 || valid != 1 || errored != 0 {
		t.Fatalf("stats total=%d valid=%d errored=%d", total, valid, errored)
	}
	if at, ok := accounts.PickAccessToken(); !ok || at != jwtA {
		t.Fatalf("pick AT ok=%t", ok)
	}
}

func TestChatGPTAccountsClearRemovesOnlyChatGPT(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(authDir, "codex-keep.json")
	if err := os.WriteFile(other, []byte(`{"type":"codex","access_token":"keep"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	mgr := coreauth.NewManager(store, nil, nil)
	accounts := NewChatGPTAccounts(mgr, authDir, nil)
	if _, err := accounts.Import([]string{jwtA}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.Clear(); err != nil {
		t.Fatal(err)
	}
	if got := len(mgr.List()); got != 0 {
		t.Fatalf("manager auths = %d after clear", got)
	}
	matches, err := filepath.Glob(filepath.Join(authDir, "chatgpt-web-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("chatgpt-web files left: %v", matches)
	}
	if _, err = os.Stat(other); err != nil {
		t.Fatalf("other provider file should stay: %v", err)
	}
}

func TestChatGPTAccountsErrorIDs(t *testing.T) {
	mgr := coreauth.NewManager(nil, nil, nil)
	accounts := NewChatGPTAccounts(mgr, "", nil)
	if _, err := accounts.Import([]string{jwtA}); err != nil {
		t.Fatal(err)
	}
	auth := mgr.List()[0]
	auth.Status = coreauth.StatusError
	if _, err := mgr.Update(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatal(err)
	}
	ids := accounts.ErrorIDs()
	if len(ids) != 1 || ids[0] != auth.ID {
		t.Fatalf("error ids = %v", ids)
	}
	if total, valid, errored := accounts.Stats(); total != 1 || valid != 0 || errored != 1 {
		t.Fatalf("stats total=%d valid=%d errored=%d", total, valid, errored)
	}
	if _, ok := accounts.PickAccessToken(); ok {
		t.Fatal("unusable auth should not be picked")
	}
}
