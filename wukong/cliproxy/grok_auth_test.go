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

// 面板按凭证文件刷新，Quota 的 id 要能同时认账号名、auth ID 和文件名。
func TestGrokAuthMatchesAcceptsIDAndFileName(t *testing.T) {
	cred := grok.Credential{SSOToken: "sso-live", Name: "plus-1"}
	auth := newGrokAuth(cred, time.Now())
	bindGrokAuthFile(auth, cred.ID(), t.TempDir())
	for _, id := range []string{"plus-1", "PLUS-1", auth.ID, auth.FileName, "grok-web-plus-1.json"} {
		if !grokAuthMatches(auth, cred, id) {
			t.Fatalf("id %q should match auth %q / file %q", id, auth.ID, auth.FileName)
		}
	}
	if grokAuthMatches(auth, cred, "other") {
		t.Fatal("unrelated id must not match")
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

// 新灌的 Grok 号默认带 grok-web 前缀，且随文件落盘。
func TestGrokAccountsImportSetsDefaultPrefix(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	mgr := coreauth.NewManager(store, nil, nil)
	accounts := NewGrokAccounts(mgr, authDir, grok.Config{}, nil)
	if _, err := accounts.ImportRaw([]byte(`{"sso":"sso-prefix","name":"p1"}`)); err != nil {
		t.Fatal(err)
	}
	auth := mgr.List()[0]
	if auth.Prefix != GrokProviderKey {
		t.Fatalf("Prefix = %q, want %q", auth.Prefix, GrokProviderKey)
	}
	raw, err := os.ReadFile(auth.Attributes[coreauth.AttributePath])
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err = json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta[modelPrefixKey] != GrokProviderKey {
		t.Fatalf("persisted prefix = %v, want %q", meta[modelPrefixKey], GrokProviderKey)
	}
}

// 启动加载时以文件为准：老文件没有 prefix 就保持无前缀。
func TestGrokRegisterAuthDirHonorsFilePrefix(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(authDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(grokWebAuthFileName("a"), `{"type":"grok-web","sso_token":"sso-a","name":"a","prefix":"grok-web"}`)
	write(grokWebAuthFileName("b"), `{"type":"grok-web","sso_token":"sso-b","name":"b"}`)

	mgr := coreauth.NewManager(nil, nil, nil)
	accounts := NewGrokAccounts(mgr, authDir, grok.Config{}, nil)
	if n, err := accounts.Load(context.Background(), filepath.Join(dir, "missing.json")); err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	for _, auth := range mgr.List() {
		switch auth.FileName {
		case grokWebAuthFileName("a"):
			if auth.Prefix != GrokProviderKey {
				t.Fatalf("a: prefix = %q", auth.Prefix)
			}
		case grokWebAuthFileName("b"):
			if auth.Prefix != "" {
				t.Fatalf("b: legacy file must stay unprefixed, got %q", auth.Prefix)
			}
		}
	}
}

// Clearance 刷新会整体替换 Metadata，前缀必须跟着保留并落盘。
func TestGrokApplyClearanceUpdateKeepsPrefix(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	mgr := coreauth.NewManager(store, nil, nil)
	accounts := NewGrokAccounts(mgr, authDir, grok.Config{}, nil)
	if _, err := accounts.ImportRaw([]byte(`{"sso":"sso-keep","name":"keep"}`)); err != nil {
		t.Fatal(err)
	}
	accounts.ApplyClearanceUpdate(grok.Credential{SSOToken: "sso-keep", CloudflareCookies: "cf=2", UserAgent: "UA2"})
	auth := mgr.List()[0]
	if auth.Prefix != GrokProviderKey || auth.Metadata[modelPrefixKey] != GrokProviderKey {
		t.Fatalf("prefix lost after clearance update: %q / %v", auth.Prefix, auth.Metadata[modelPrefixKey])
	}
	raw, err := os.ReadFile(auth.Attributes[coreauth.AttributePath])
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err = json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta[modelPrefixKey] != GrokProviderKey || meta["cloudflare_cookies"] != "cf=2" {
		t.Fatalf("persisted %#v", meta)
	}
}

// 重灌同一个 Grok 账号：SSO / cookies 等凭证字段以新值为准，面板配置与停用状态保留。
func TestGrokAccountsReimportKeepsUserSettings(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	mgr := coreauth.NewManager(store, nil, nil)
	accounts := NewGrokAccounts(mgr, authDir, grok.Config{}, nil)
	if _, err := accounts.ImportRaw([]byte(`{"sso":"sso-v1","name":"p1","cloudflare_cookies":"cf=old"}`)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	auth := mgr.List()[0]
	auth.Metadata["note"] = "备用号"
	auth.Metadata["proxy_url"] = "socks5://127.0.0.1:1080"
	auth.ProxyURL = "socks5://127.0.0.1:1080"
	auth.Disabled = true
	auth.Status = coreauth.StatusDisabled
	if _, err := mgr.Update(ctx, auth); err != nil {
		t.Fatal(err)
	}

	// 同名账号（ID 由 name 派生）换了 SSO，且这次没带 cookies
	if _, err := accounts.ImportRaw([]byte(`{"sso":"sso-v2","name":"p1"}`)); err != nil {
		t.Fatal(err)
	}
	if accounts.Count() != 1 {
		t.Fatalf("count=%d, want 1", accounts.Count())
	}
	got := mgr.List()[0]
	if got.Metadata["sso_token"] != "sso-v2" || got.Attributes["sso_token"] != "sso-v2" {
		t.Fatalf("sso not updated: meta=%#v attrs=%#v", got.Metadata, got.Attributes)
	}
	if _, stale := got.Metadata["cloudflare_cookies"]; stale {
		t.Fatalf("stale cookies must not be inherited: %#v", got.Metadata)
	}
	if got.Metadata["note"] != "备用号" || got.ProxyURL != "socks5://127.0.0.1:1080" || !got.Disabled {
		t.Fatalf("user settings lost: note=%v proxy=%q disabled=%v", got.Metadata["note"], got.ProxyURL, got.Disabled)
	}
	if got.Prefix != GrokProviderKey {
		t.Fatalf("prefix = %q", got.Prefix)
	}
	raw, err := os.ReadFile(got.Attributes[coreauth.AttributePath])
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err = json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["note"] != "备用号" || meta["sso_token"] != "sso-v2" || meta["disabled"] != true {
		t.Fatalf("persisted %#v", meta)
	}
}

// Clearance 刷新只应改 Grok 自己维护的字段，面板里写的 note / proxy_url 要留下。
func TestGrokApplyClearanceUpdateKeepsUserMetadata(t *testing.T) {
	mgr := coreauth.NewManager(nil, nil, nil)
	accounts := NewGrokAccounts(mgr, "", grok.Config{}, nil)
	if _, err := accounts.ImportRaw([]byte(`{"sso":"sso-meta","name":"m1","email":"old@example.com"}`)); err != nil {
		t.Fatal(err)
	}
	auth := mgr.List()[0]
	auth.Metadata["note"] = "别删我"
	auth.Metadata["proxy_url"] = "http://127.0.0.1:7890"
	if _, err := mgr.Update(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatal(err)
	}
	accounts.ApplyClearanceUpdate(grok.Credential{SSOToken: "sso-meta", CloudflareCookies: "cf=3", UserAgent: "UA3"})
	got := mgr.List()[0]
	if got.Metadata["cloudflare_cookies"] != "cf=3" || got.Metadata["user_agent"] != "UA3" {
		t.Fatalf("clearance not applied: %#v", got.Metadata)
	}
	if got.Metadata["email"] != "old@example.com" {
		t.Fatalf("existing credential fields should be re-applied from the credential: %#v", got.Metadata)
	}
	if got.Metadata["note"] != "别删我" || got.Metadata["proxy_url"] != "http://127.0.0.1:7890" {
		t.Fatalf("user metadata lost on clearance update: %#v", got.Metadata)
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
