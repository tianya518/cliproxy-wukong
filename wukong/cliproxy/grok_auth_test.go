package cliproxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"

	"github.com/router-for-me/CLIProxyAPI/v7/wukong/grok"
)

func TestReplaceGrokAuthsKeepsChatGPT(t *testing.T) {
	ctx := context.Background()
	mgr := coreauth.NewManager(nil, nil, nil)
	_, err := mgr.Register(ctx, &coreauth.Auth{
		ID:       ProviderKey + "-keep",
		Provider: ProviderKey,
		Status:   coreauth.StatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = RegisterGrokAuths(ctx, mgr, []grok.Credential{{SSOToken: "old-sso", Name: "old"}}); err != nil {
		t.Fatal(err)
	}
	if err = ReplaceGrokAuths(ctx, mgr, []grok.Credential{{SSOToken: "new-sso", Name: "new"}}); err != nil {
		t.Fatal(err)
	}

	var chatgpt, grokN int
	for _, auth := range mgr.List() {
		switch auth.Provider {
		case ProviderKey:
			chatgpt++
			if auth.ID != ProviderKey+"-keep" {
				t.Fatalf("chatgpt auth mutated: %s", auth.ID)
			}
		case GrokProviderKey:
			grokN++
			if auth.Attributes["sso_token"] != "new-sso" {
				t.Fatalf("grok auth %#v", auth)
			}
		}
	}
	if chatgpt != 1 || grokN != 1 {
		t.Fatalf("chatgpt=%d grok=%d", chatgpt, grokN)
	}
}

func TestGrokAccountsImportWritesAuthDir(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	mgr := coreauth.NewManager(store, nil, nil)
	accounts := NewGrokAccounts(mgr, authDir, grok.Config{}, nil)
	added, err := accounts.ImportRaw([]byte(`{"sso":"sso-live","name":"plus-1"}`))
	if err != nil || added != 1 {
		t.Fatalf("import added=%d err=%v", added, err)
	}
	if accounts.Count() != 1 {
		t.Fatalf("count=%d", accounts.Count())
	}
	pubs := accounts.PublicAccounts()
	if len(pubs) != 1 || pubs[0].Name != "plus-1" || !pubs[0].HasSSO {
		t.Fatalf("public %#v", pubs)
	}
	auth := mgr.List()[0]
	raw, err := os.ReadFile(auth.Attributes[coreauth.AttributePath])
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err = json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["type"] != GrokProviderKey || meta["sso_token"] != "sso-live" {
		t.Fatalf("persisted %#v", meta)
	}
}

func TestGrokAccountsClearLeavesChatGPT(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(authDir, "chatgpt-web-keep.json")
	if err := os.WriteFile(keep, []byte(`{"type":"chatgpt-web","access_token":"keep"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	mgr := coreauth.NewManager(store, nil, nil)
	if _, err := mgr.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{
		ID: ProviderKey + "-keep", Provider: ProviderKey, Status: coreauth.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	accounts := NewGrokAccounts(mgr, authDir, grok.Config{}, nil)
	if _, err := accounts.ImportRaw([]byte(`{"sso":"sso-clear","name":"g1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := accounts.Clear(); err != nil {
		t.Fatal(err)
	}
	if accounts.Count() != 0 {
		t.Fatal("grok should be empty")
	}
	var chatgpt int
	for _, auth := range mgr.List() {
		if auth.Provider == ProviderKey {
			chatgpt++
		}
	}
	if chatgpt != 1 {
		t.Fatalf("chatgpt=%d", chatgpt)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("chatgpt file should stay: %v", err)
	}
}

func TestGrokAccountsLoadMigratesFile(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "grok.json")
	if err := grok.SaveCredentialsFile(src, []grok.Credential{{SSOToken: "sso-mig", Name: "mig"}}); err != nil {
		t.Fatal(err)
	}
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	mgr := coreauth.NewManager(store, nil, nil)
	accounts := NewGrokAccounts(mgr, authDir, grok.Config{}, nil)
	n, err := accounts.Load(context.Background(), src)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	path := filepath.Join(authDir, grokWebAuthFileName("mig"))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err = json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["sso_token"] != "sso-mig" {
		t.Fatalf("migrated %#v", meta)
	}
	n, err = accounts.Load(context.Background(), src)
	if err != nil || n != 1 {
		t.Fatalf("second load should skip migrate, n=%d err=%v", n, err)
	}
}

func TestGrokAccountsApplyClearanceUpdate(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	mgr := coreauth.NewManager(store, nil, nil)
	accounts := NewGrokAccounts(mgr, authDir, grok.Config{}, nil)
	if _, err := accounts.ImportRaw([]byte(`{"sso":"sso-cf","name":"cf"}`)); err != nil {
		t.Fatal(err)
	}
	accounts.ApplyClearanceUpdate(grok.Credential{SSOToken: "sso-cf", CloudflareCookies: "cf=1", UserAgent: "UA"})
	auth := mgr.List()[0]
	if auth.Metadata["cloudflare_cookies"] != "cf=1" || auth.Attributes["user_agent"] != "UA" {
		t.Fatalf("metadata %#v attrs %#v", auth.Metadata, auth.Attributes)
	}
	raw, err := os.ReadFile(auth.Attributes[coreauth.AttributePath])
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err = json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["cloudflare_cookies"] != "cf=1" {
		t.Fatalf("persisted %#v", meta)
	}
}
