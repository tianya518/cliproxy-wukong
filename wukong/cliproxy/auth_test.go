package cliproxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"

	sentinelserver "github.com/router-for-me/CLIProxyAPI/v7/wukong/server"
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
	if at, ok := accounts.PickAccessToken(); !ok || at != jwtA {
		t.Fatal("a still-valid AT should stay pickable after a false status error")
	}
}

func TestChatGPTAccountsMarkCatalogError(t *testing.T) {
	mgr := coreauth.NewManager(nil, nil, nil)
	accounts := NewChatGPTAccounts(mgr, "", nil)
	if _, err := accounts.Import([]string{jwtA}); err != nil {
		t.Fatal(err)
	}
	auth := mgr.List()[0]
	accounts.MarkCatalogError(auth.ID, "模型目录同步失败（token 无效）：http 401")
	got := mgr.List()[0]
	if got.Status != coreauth.StatusError {
		t.Fatalf("status = %q", got.Status)
	}
	if !strings.Contains(got.StatusMessage, "http 401") {
		t.Fatalf("status_message = %q", got.StatusMessage)
	}
	token, _, ok := accounts.PrepareCatalogToken()
	if !ok || token != jwtA {
		t.Fatalf("fresh AT should still be used after catalog error, ok=%t", ok)
	}
	if cleared := mgr.List()[0]; cleared.Status != coreauth.StatusActive || cleared.StatusMessage != "" {
		t.Fatalf("status=%q message=%q", cleared.Status, cleared.StatusMessage)
	}
}

// 新灌的号要默认带 chatgpt-web 前缀：运行时字段与落盘字段都得有，模型才会以
// chatgpt-web/<model> 注册并且重启后仍然如此。
func TestChatGPTAccountsImportSetsDefaultPrefix(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	mgr := coreauth.NewManager(store, nil, nil)
	accounts := NewChatGPTAccounts(mgr, authDir, nil)
	if _, err := accounts.Import([]string{jwtA}); err != nil {
		t.Fatal(err)
	}
	auth := mgr.List()[0]
	if auth.Prefix != ProviderKey {
		t.Fatalf("Prefix = %q, want %q", auth.Prefix, ProviderKey)
	}
	raw, err := os.ReadFile(auth.Attributes[coreauth.AttributePath])
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err = json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta[modelPrefixKey] != ProviderKey {
		t.Fatalf("persisted prefix = %v, want %q; meta=%#v", meta[modelPrefixKey], ProviderKey, meta)
	}
}

// 重灌同一个账号不能覆盖用户在面板里改过的前缀。
func TestChatGPTAccountsReimportKeepsCustomPrefix(t *testing.T) {
	mgr := coreauth.NewManager(nil, nil, nil)
	accounts := NewChatGPTAccounts(mgr, "", nil)
	if _, err := accounts.Import([]string{jwtA}); err != nil {
		t.Fatal(err)
	}
	auth := mgr.List()[0]
	setModelPrefix(auth, "team-a")
	if _, err := mgr.Update(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.Import([]string{jwtA}); err != nil {
		t.Fatal(err)
	}
	got := mgr.List()[0]
	if got.Prefix != "team-a" || got.Metadata[modelPrefixKey] != "team-a" {
		t.Fatalf("prefix after reimport = %q / %v, want team-a", got.Prefix, got.Metadata[modelPrefixKey])
	}
}

// 启动加载 auth-dir 时以文件为准：写了 prefix 的带上，老文件没写的不替用户补默认值。
func TestRegisterAuthDirHonorsFilePrefix(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, meta map[string]any) {
		raw, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(authDir, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(chatgptWebAuthFileName("with-prefix"), map[string]any{
		"type": ProviderKey, "access_token": jwtA, modelPrefixKey: " /web/ ",
	})
	write(chatgptWebAuthFileName("legacy"), map[string]any{
		"type": ProviderKey, "access_token": jwtB,
	})

	mgr := coreauth.NewManager(nil, nil, nil)
	accounts := NewChatGPTAccounts(mgr, authDir, nil)
	if n, err := accounts.Load(context.Background(), filepath.Join(dir, "missing.json")); err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	prefixes := map[string]string{}
	for _, auth := range mgr.List() {
		prefixes[auth.FileName] = auth.Prefix
	}
	if got := prefixes[chatgptWebAuthFileName("with-prefix")]; got != "web" {
		t.Fatalf("prefix from file = %q, want web (trimmed)", got)
	}
	if got := prefixes[chatgptWebAuthFileName("legacy")]; got != "" {
		t.Fatalf("legacy file without prefix must stay unprefixed, got %q", got)
	}
}

// 重灌同一账号只更新凭证：面板里配的 note / proxy_url / weight / 停用状态等必须留在文件里，
// 而旧的 session_token 这类凭证字段不能从旧记录里继承回来。
func TestChatGPTAccountsReimportKeepsUserSettings(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	mgr := coreauth.NewManager(store, nil, nil)
	accounts := NewChatGPTAccounts(mgr, authDir, nil)

	// 上传解析不认 id 字段（ID 由 token 哈希得出），这里直接走 upsert 以固定账号 ID，
	// 与旧 chatgpt.json 迁移路径一致。
	ctx := context.Background()
	if err := accounts.upsert(ctx, sentinelserver.Credential{ID: "acct-1", AccessToken: jwtA, SessionToken: "st-old"}, true); err != nil {
		t.Fatal(err)
	}
	// 模拟用户在面板里改过这张卡
	auth := mgr.List()[0]
	auth.Metadata["note"] = "老板的号"
	auth.Metadata["proxy_url"] = "http://127.0.0.1:7890"
	auth.Metadata["weight"] = float64(3)
	auth.Metadata["excluded_models"] = []any{"gpt-5-6-mini"}
	auth.ProxyURL = "http://127.0.0.1:7890"
	auth.Disabled = true
	auth.Status = coreauth.StatusDisabled
	auth.Attributes["model_aliases"] = `[{"name":"gpt-5-6","alias":"g56"}]`
	if _, err := mgr.Update(ctx, auth); err != nil {
		t.Fatal(err)
	}
	created := auth.CreatedAt

	if err := accounts.upsert(ctx, sentinelserver.Credential{ID: "acct-1", AccessToken: jwtB, RefreshToken: "rt-new"}, true); err != nil {
		t.Fatal(err)
	}
	if got := len(mgr.List()); got != 1 {
		t.Fatalf("manager auths = %d, want 1", got)
	}
	got := mgr.List()[0]
	if got.Metadata["access_token"] != jwtB || got.Metadata["refresh_token"] != "rt-new" {
		t.Fatalf("credential not updated: %#v", got.Metadata)
	}
	if _, stale := got.Metadata["session_token"]; stale {
		t.Fatalf("stale session_token must not be inherited: %#v", got.Metadata)
	}
	if got.Metadata["note"] != "老板的号" || got.Metadata["proxy_url"] != "http://127.0.0.1:7890" || got.Metadata["weight"] != float64(3) {
		t.Fatalf("user metadata lost: %#v", got.Metadata)
	}
	if got.ProxyURL != "http://127.0.0.1:7890" {
		t.Fatalf("ProxyURL = %q", got.ProxyURL)
	}
	if !got.Disabled || got.Status != coreauth.StatusDisabled {
		t.Fatalf("operator disable must survive reimport: disabled=%v status=%q", got.Disabled, got.Status)
	}
	if got.Attributes["model_aliases"] == "" {
		t.Fatalf("derived attributes lost: %#v", got.Attributes)
	}
	if !got.CreatedAt.Equal(created) {
		t.Fatalf("CreatedAt changed: %v -> %v", created, got.CreatedAt)
	}
	if got.Prefix != ProviderKey {
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
	if meta["note"] != "老板的号" || meta["proxy_url"] != "http://127.0.0.1:7890" || meta["disabled"] != true {
		t.Fatalf("persisted %#v", meta)
	}
	if meta["access_token"] != jwtB {
		t.Fatalf("persisted access_token = %v", meta["access_token"])
	}
}

// RT-only 重灌不能从旧记录的 Attributes 里捡回过期的 access token。
func TestChatGPTAccountsReimportDoesNotInheritStaleAccessToken(t *testing.T) {
	mgr := coreauth.NewManager(nil, nil, nil)
	accounts := NewChatGPTAccounts(mgr, "", nil)
	ctx := context.Background()
	if err := accounts.upsert(ctx, sentinelserver.Credential{ID: "acct-1", AccessToken: jwtA}, false); err != nil {
		t.Fatal(err)
	}
	if err := accounts.upsert(ctx, sentinelserver.Credential{ID: "acct-1", RefreshToken: "rt-only"}, false); err != nil {
		t.Fatal(err)
	}
	if got := len(mgr.List()); got != 1 {
		t.Fatalf("manager auths = %d, want 1", got)
	}
	got := mgr.List()[0]
	if _, err := accessTokenFrom(got); err == nil {
		t.Fatalf("RT-only reimport must not expose the old AT: meta=%#v attrs=%#v", got.Metadata, got.Attributes)
	}
	if got.Attributes[coreauth.AttributeAuthKind] != coreauth.AuthKindOAuth {
		t.Fatalf("auth_kind = %q", got.Attributes[coreauth.AttributeAuthKind])
	}
}

func TestNormalizeModelPrefix(t *testing.T) {
	cases := map[string]string{
		"chatgpt-web":   "chatgpt-web",
		" /grok-web/ ":  "grok-web",
		"":              "",
		"   ":           "",
		"team/a":        "", // 含斜杠与 cliproxy 同步器一样视为无效
		"/":             "",
		"chatgpt-web//": "chatgpt-web",
	}
	for in, want := range cases {
		if got := normalizeModelPrefix(in); got != want {
			t.Errorf("normalizeModelPrefix(%q) = %q, want %q", in, got, want)
		}
	}
	if got := modelPrefixOf(&coreauth.Auth{Metadata: map[string]any{modelPrefixKey: "from-meta"}}); got != "from-meta" {
		t.Errorf("modelPrefixOf should fall back to metadata, got %q", got)
	}
	if got := modelPrefixOf(&coreauth.Auth{Prefix: "runtime", Metadata: map[string]any{modelPrefixKey: "from-meta"}}); got != "runtime" {
		t.Errorf("modelPrefixOf should prefer Auth.Prefix, got %q", got)
	}
}

func TestPrepareCatalogTokenUsesExistingAT(t *testing.T) {
	mgr := coreauth.NewManager(nil, nil, nil)
	accounts := NewChatGPTAccounts(mgr, "", nil)
	if _, err := accounts.Import([]string{jwtA}); err != nil {
		t.Fatal(err)
	}
	token, id, ok := accounts.PrepareCatalogToken()
	if !ok || token != jwtA || id == "" {
		t.Fatalf("token=%q id=%q ok=%t", token, id, ok)
	}
}
