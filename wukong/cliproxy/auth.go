package cliproxy

// auth.go — read ChatGPT tokens from cliproxy Auth records and register
// chatgpt.json entries into the cliproxy credential pool.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"

	sentinelserver "github.com/router-for-me/CLIProxyAPI/v7/wukong/server"
)

const chatgptWebRefreshLead = 24 * time.Hour

// 网页逆向的模型名默认带 provider 前缀（chatgpt-web/gpt-5-6-thinking、grok-web/grok-chat-auto），
// 这样在 /v1/models 里能和 codex / xai 等官方 API 模型（gpt-5.5、gpt-image-2、grok-4.6）一眼分开。
//
// 用的是 cliproxy 自带的凭证前缀机制：Auth.Prefix 非空时模型注册为 <prefix>/<model>，
// 请求到达 executor 前由 rewriteModelForAuth 剥掉前缀（sdk/cliproxy/auth/conductor_models.go）。
// 配合 config 的 force-model-prefix: true，无前缀旧名就不再出现。
//
// 前缀只在 wukong 自己创建凭证（/chatgpt/upload、/grok/upload、旧文件一次性迁移）时写入；
// 启动加载 auth-dir 时以文件里的 prefix 字段为准，不替用户补默认值——cliproxy 的文件同步器
// 也会读同一个文件，两边必须一致，否则模型表会在有/无前缀之间来回跳。
const (
	defaultChatGPTModelPrefix = ProviderKey
	defaultGrokModelPrefix    = GrokProviderKey

	// modelPrefixKey 是凭证文件里前缀字段名，与 internal/watcher/synthesizer 读取的键一致。
	modelPrefixKey = "prefix"
)

// modelPrefixFromMeta 按 cliproxy 文件同步器同样的规则读前缀：去空白与首尾斜杠，内含斜杠视为无效。
func modelPrefixFromMeta(meta map[string]any) string {
	raw, _ := meta[modelPrefixKey].(string)
	return normalizeModelPrefix(raw)
}

func normalizeModelPrefix(raw string) string {
	trimmed := strings.Trim(strings.TrimSpace(raw), "/")
	if trimmed == "" || strings.Contains(trimmed, "/") {
		return ""
	}
	return trimmed
}

// modelPrefixOf 取凭证当前生效的前缀：优先运行时字段，其次落盘的 metadata。
func modelPrefixOf(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if p := normalizeModelPrefix(auth.Prefix); p != "" {
		return p
	}
	return modelPrefixFromMeta(auth.Metadata)
}

// setModelPrefix 同时写 Auth.Prefix（路由/模型注册用）和 Metadata["prefix"]（落盘用）。
// 两处必须一致：FileTokenStore 只序列化 Metadata，而模型注册只看 Auth.Prefix。空前缀不做改动。
func setModelPrefix(auth *coreauth.Auth, prefix string) {
	if auth == nil {
		return
	}
	prefix = normalizeModelPrefix(prefix)
	if prefix == "" {
		return
	}
	auth.Prefix = prefix
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[modelPrefixKey] = prefix
}

// importModelPrefix 决定导入/重导入一个账号时用的前缀：已有凭证保留用户自定义值，否则用默认。
func importModelPrefix(existing *coreauth.Auth, fallback string) string {
	if p := modelPrefixOf(existing); p != "" {
		return p
	}
	return fallback
}

// chatgptManagedMetaKeys 是 wukong 自己维护的 ChatGPT 凭证字段。重灌同一账号时这些键整体以新值为准，
// 旧值不继承（新凭证没带 session_token 就不该留着旧的过期值）。其余键——note、proxy_url、weight、
// priority、headers、excluded_models、model_aliases 这些用户在面板里配的——一律保留，
// 与上游 OAuth 重新登录时 MergeExistingAuthMetadata 的做法一致。
var chatgptManagedMetaKeys = []string{
	"type", "access_token", "accessToken", "api_key",
	"refresh_token", "refreshToken", "session_token", "sessionToken",
	"expired", "expires_at", "expiresAt",
}

// grokManagedMetaKeys 同上，Grok 侧由上传/会话探测/Clearance 刷新维护的字段。
var grokManagedMetaKeys = []string{
	"type", "sso_token", "token", "name", "user_id", "userId", "email",
	"cloudflare_cookies", "cf_cookies", "user_agent", "tier",
}

func isManagedMetaKey(key string, managed []string) bool {
	for _, k := range managed {
		if strings.EqualFold(strings.TrimSpace(key), k) {
			return true
		}
	}
	return false
}

// carryOverUserSettings 把旧记录里用户配置的部分搬到重建的新记录上：
//   - Metadata 里非 managed 的键（面板写入的 note / proxy_url / weight / excluded_models …，含 prefix）；
//   - Attributes 里新记录没设的键（同步器从上述字段派生的别名、排除表等）；
//   - 由它们派生的运行时字段 Prefix / ProxyURL / Disabled，以及 CreatedAt。
//
// Status 不继承：重灌意味着拿到了新凭证，先按可用处理，坏了由后续请求/巡检再标错。
// Disabled 继承：那是操作者的明确选择，批量重贴一批 token 不应顺手把停用的号开回去。
func carryOverUserSettings(next, existing *coreauth.Auth, managed []string) {
	if next == nil || existing == nil {
		return
	}
	if len(existing.Metadata) > 0 {
		carry := make(map[string]any, len(existing.Metadata))
		for k, v := range existing.Metadata {
			if isManagedMetaKey(k, managed) {
				continue
			}
			carry[k] = v
		}
		coreauth.MergeExistingAuthMetadata(next, carry)
	}
	if len(existing.Attributes) > 0 {
		if next.Attributes == nil {
			next.Attributes = make(map[string]string, len(existing.Attributes))
		}
		for k, v := range existing.Attributes {
			// 凭证本体与由它推出的 auth_kind 只认新记录：RT-only 重灌时若把旧的 api_key 带过来，
			// accessTokenFrom 会捡到过期 AT。
			if isManagedMetaKey(k, managed) || k == "api_key" || k == coreauth.AttributeAuthKind {
				continue
			}
			if _, set := next.Attributes[k]; !set {
				next.Attributes[k] = v
			}
		}
	}
	if next.Prefix == "" {
		next.Prefix = existing.Prefix
	}
	if next.ProxyURL == "" {
		next.ProxyURL = existing.ProxyURL
	}
	if existing.Disabled {
		next.Disabled = true
		next.Status = coreauth.StatusDisabled
	}
	if !existing.CreatedAt.IsZero() {
		next.CreatedAt = existing.CreatedAt
	}
}

// replaceManagedMetadata 用 fresh 里的 managed 字段覆盖 auth.Metadata，其余键原样保留。
// 给"就地刷新"的场景用（如 Grok Clearance 更新）：整体替换 Metadata 会把面板配置一并抹掉。
func replaceManagedMetadata(auth *coreauth.Auth, fresh map[string]any, managed []string) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any, len(fresh))
	}
	for k := range auth.Metadata {
		if isManagedMetaKey(k, managed) {
			delete(auth.Metadata, k)
		}
	}
	for k, v := range fresh {
		auth.Metadata[k] = v
	}
}

func init() {
	lead := chatgptWebRefreshLead
	coreauth.RegisterRefreshLeadProvider(ProviderKey, func() *time.Duration {
		d := lead
		return &d
	})
}

// Credential fields on an Auth record, in lookup order.
// Metadata holds mutable state (refreshed AT / RT / ST). Attributes hold the
// static copy written at registration.
var (
	attributeKeys = []string{"api_key", "access_token", "accessToken"}
	metadataKeys  = []string{"access_token", "accessToken", "api_key"}
)

// accessTokenFrom extracts a usable ChatGPT access token from Auth.
func accessTokenFrom(auth *coreauth.Auth) (string, error) {
	if auth == nil {
		return "", requestError{fmt.Errorf("%s: no credential selected", ProviderKey)}
	}
	for _, k := range metadataKeys {
		if s, ok := auth.Metadata[k].(string); ok && strings.TrimSpace(s) != "" {
			return normalizeCredential(s)
		}
	}
	for _, k := range attributeKeys {
		if v := strings.TrimSpace(auth.Attributes[k]); v != "" {
			return normalizeCredential(v)
		}
	}
	if hasRefreshMaterial(auth) {
		return "", upstreamError{
			err:  fmt.Errorf("%s: auth %q has no access token; refresh required", ProviderKey, auth.ID),
			code: 401,
		}
	}
	return "", fmt.Errorf("%s: auth %q carries no access token", ProviderKey, auth.ID)
}

func hasRefreshMaterial(auth *coreauth.Auth) bool {
	return metadataString(auth, "refresh_token", "refreshToken") != "" ||
		metadataString(auth, "session_token", "sessionToken") != ""
}

func metadataString(auth *coreauth.Auth, keys ...string) string {
	if auth == nil {
		return ""
	}
	for _, key := range keys {
		if s, ok := auth.Metadata[key].(string); ok {
			if v := strings.TrimSpace(s); v != "" {
				return v
			}
		}
		if v := strings.TrimSpace(auth.Attributes[key]); v != "" {
			return v
		}
	}
	return ""
}

// normalizeCredential accepts the credential spellings wukong already supports
// (session JSON, <access>----<session>, rt:<refresh>, raw token).
func normalizeCredential(raw string) (string, error) {
	cred, ok := sentinelserver.ParseCredential(raw)
	if !ok || cred.AccessToken == "" {
		return "", fmt.Errorf("%s: credential has no usable access token", ProviderKey)
	}
	return cred.AccessToken, nil
}

// ChatGPTAccounts imports ChatGPT web credentials into cliproxy's auth-dir
// and runtime Manager. /chatgpt/upload and startup migration both go through it.
type ChatGPTAccounts struct {
	mgr      *coreauth.Manager
	authDir  string
	onChange func()
}

// NewChatGPTAccounts wires ChatGPT account import to the same auth-dir
// FileTokenStore that Codex/Claude use. Status, catalog pick, and check
// also read this Manager — there is no second TokenPool on the gateway path.
func NewChatGPTAccounts(mgr *coreauth.Manager, authDir string, onChange func()) *ChatGPTAccounts {
	return &ChatGPTAccounts{mgr: mgr, authDir: strings.TrimSpace(authDir), onChange: onChange}
}

var _ sentinelserver.ChatGPTAccountAdmin = (*ChatGPTAccounts)(nil)

func (s *ChatGPTAccounts) notify() {
	if s != nil && s.onChange != nil {
		s.onChange()
	}
}

// Import parses pasted tokens, writes auth-dir/chatgpt-web-<id>.json, and
// registers them into the cliproxy Manager.
func (s *ChatGPTAccounts) Import(chunks []string) (int, error) {
	if s == nil || s.mgr == nil {
		return 0, fmt.Errorf("%s: account store is not configured", ProviderKey)
	}
	ctx := context.Background()
	added := 0
	for _, chunk := range chunks {
		creds := sentinelserver.ParseUploadCredentials(chunk)
		if len(creds) == 0 {
			if cred, ok := sentinelserver.ParseCredential(chunk); ok {
				creds = []sentinelserver.Credential{cred}
			}
		}
		for _, cred := range creds {
			if err := s.upsert(ctx, cred, true); err != nil {
				return added, err
			}
			added++
		}
	}
	if added > 0 {
		s.notify()
	}
	return added, nil
}

// Clear removes chatgpt-web auths from the runtime pool and deletes their
// auth-dir files. Other providers are left untouched.
func (s *ChatGPTAccounts) Clear() error {
	if s == nil || s.mgr == nil {
		return fmt.Errorf("%s: account store is not configured", ProviderKey)
	}
	ctx := context.Background()
	for _, auth := range s.mgr.List() {
		if auth == nil || auth.Provider != ProviderKey {
			continue
		}
		if path := strings.TrimSpace(auth.Attributes[coreauth.AttributePath]); path != "" {
			_ = os.Remove(path)
		}
		s.mgr.Remove(ctx, auth.ID)
	}
	if s.authDir != "" {
		if err := removeChatGPTAuthFiles(s.authDir); err != nil {
			return err
		}
	}
	s.notify()
	return nil
}

func (s *ChatGPTAccounts) each(fn func(*coreauth.Auth)) {
	if s == nil || s.mgr == nil {
		return
	}
	for _, auth := range s.mgr.List() {
		if auth == nil || auth.Provider != ProviderKey {
			continue
		}
		fn(auth)
	}
}

func authUnusable(auth *coreauth.Auth) bool {
	return auth == nil || auth.Disabled ||
		auth.Status == coreauth.StatusDisabled ||
		auth.Status == coreauth.StatusError
}

// Stats counts chatgpt-web auths in the Manager.
func (s *ChatGPTAccounts) Stats() (total, valid, errored int) {
	s.each(func(auth *coreauth.Auth) {
		total++
		if authUnusable(auth) {
			errored++
			return
		}
		if _, err := accessTokenFrom(auth); err == nil || hasRefreshMaterial(auth) {
			valid++
			return
		}
		errored++
	})
	return total, valid, errored
}

// ErrorIDs returns Manager IDs that are disabled or in error.
func (s *ChatGPTAccounts) ErrorIDs() []string {
	var ids []string
	s.each(func(auth *coreauth.Auth) {
		if authUnusable(auth) {
			ids = append(ids, auth.ID)
		}
	})
	return ids
}

// PickAccessToken returns one usable ChatGPT access token for catalog sync.
// It does not hit the network.
func (s *ChatGPTAccounts) PickAccessToken() (string, bool) {
	token, _, _ := s.pickCatalogAuth()
	return token, token != ""
}

// PrepareCatalogToken picks a usable chatgpt-web auth for /backend-api/models.
// A still-valid access token is used as-is. Session / OAuth refresh only runs
// when there is no AT. A previous false 403 on the card is cleared if the AT
// is usable.
func (s *ChatGPTAccounts) PrepareCatalogToken() (token, authID string, ok bool) {
	at, auth, ok := s.pickCatalogAuth()
	if !ok || auth == nil {
		return "", "", false
	}
	if at != "" {
		s.clearAuthError(context.Background(), auth)
		return at, auth.ID, true
	}
	if !hasRefreshMaterial(auth) {
		return "", "", false
	}
	result, updated := sentinelserver.CheckChatGPTCredential(credentialFromAuth(auth))
	if !result.Valid {
		s.markAuthError(context.Background(), auth, result.Error)
		return "", "", false
	}
	if result.Refreshed {
		applyChatGPTTokens(auth, updated.AccessToken, updated.RefreshToken, updated.SessionToken, updated.ExpiresAt)
		auth.Status = coreauth.StatusActive
		auth.StatusMessage = ""
		_, _ = s.mgr.Update(context.Background(), auth)
		fresh, err := accessTokenFrom(auth)
		if err != nil || fresh == "" {
			return "", "", false
		}
		at = fresh
	}
	return at, auth.ID, at != ""
}

func (s *ChatGPTAccounts) pickCatalogAuth() (string, *coreauth.Auth, bool) {
	var picked *coreauth.Auth
	var token string
	s.each(func(auth *coreauth.Auth) {
		if picked != nil || auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			return
		}
		at, err := accessTokenFrom(auth)
		if err == nil && at != "" {
			picked = auth
			token = at
			return
		}
		if authUnusable(auth) {
			return
		}
		if hasRefreshMaterial(auth) {
			picked = auth
		}
	})
	if picked == nil {
		return "", nil, false
	}
	return token, picked, true
}

// MarkCatalogError writes a catalog 401/403 onto the auth that was used so
// the management card shows a warning instead of looking healthy.
func (s *ChatGPTAccounts) MarkCatalogError(authID, message string) {
	if s == nil || s.mgr == nil || strings.TrimSpace(authID) == "" {
		return
	}
	ctx := context.Background()
	s.each(func(auth *coreauth.Auth) {
		if auth.ID != authID && auth.FileName != authID {
			return
		}
		s.markAuthError(ctx, auth, message)
	})
}

func (s *ChatGPTAccounts) markAuthError(ctx context.Context, auth *coreauth.Auth, message string) {
	if auth == nil {
		return
	}
	auth.Status = coreauth.StatusError
	auth.StatusMessage = strings.TrimSpace(message)
	if auth.StatusMessage == "" {
		auth.StatusMessage = "chatgpt session invalid"
	}
	_, _ = s.mgr.Update(ctx, auth)
}

func (s *ChatGPTAccounts) clearAuthError(ctx context.Context, auth *coreauth.Auth) {
	if auth == nil || (auth.Status != coreauth.StatusError && auth.StatusMessage == "") {
		return
	}
	auth.Status = coreauth.StatusActive
	auth.StatusMessage = ""
	_, _ = s.mgr.Update(ctx, auth)
}

// CheckAll probes each chatgpt-web auth. Successful session/OAuth refreshes
// are written back through Manager (auth-dir).
func (s *ChatGPTAccounts) CheckAll() []sentinelserver.TokenCheckResult {
	if s == nil || s.mgr == nil {
		return nil
	}
	ctx := context.Background()
	var out []sentinelserver.TokenCheckResult
	s.each(func(auth *coreauth.Auth) {
		result, updated := sentinelserver.CheckChatGPTCredential(credentialFromAuth(auth))
		if result.ID == "" {
			result.ID = auth.ID
		}
		if result.Valid && result.Refreshed {
			applyChatGPTTokens(auth, updated.AccessToken, updated.RefreshToken, updated.SessionToken, updated.ExpiresAt)
		}
		if result.Valid {
			auth.Status = coreauth.StatusActive
			auth.StatusMessage = ""
			_, _ = s.mgr.Update(ctx, auth)
		} else {
			s.markAuthError(ctx, auth, result.Error)
		}
		out = append(out, result)
	})
	return out
}

func credentialFromAuth(auth *coreauth.Auth) sentinelserver.Credential {
	if auth == nil {
		return sentinelserver.Credential{}
	}
	cred := sentinelserver.Credential{ID: auth.ID}
	cred.AccessToken = metadataString(auth, "access_token", "accessToken", "api_key")
	cred.RefreshToken = metadataString(auth, "refresh_token", "refreshToken")
	cred.SessionToken = metadataString(auth, "session_token", "sessionToken")
	if auth.Metadata != nil {
		if exp, ok := parseMetaTime(auth.Metadata["expired"], auth.Metadata["expires_at"], auth.Metadata["expiresAt"]); ok {
			cred.ExpiresAt = exp
		}
	}
	return cred
}

// Load registers existing auth-dir chatgpt-web files, then migrates leftover
// chatgpt.json entries into auth-dir (one-time import).
func (s *ChatGPTAccounts) Load(ctx context.Context, chatgptFile string) (int, error) {
	if s == nil || s.mgr == nil {
		return 0, fmt.Errorf("%s: account store is not configured", ProviderKey)
	}
	n, err := s.registerAuthDir(ctx)
	if err != nil {
		return n, err
	}
	migrated, err := s.migrateChatGPTFile(ctx, chatgptFile)
	return n + migrated, err
}

// RegisterAuthsFromChatGPTFile keeps the old name. Prefer ChatGPTAccounts.Load.
func RegisterAuthsFromChatGPTFile(ctx context.Context, mgr *coreauth.Manager, path, authDir string) (int, error) {
	return NewChatGPTAccounts(mgr, authDir, nil).Load(ctx, path)
}

func expandChatGPTCredential(c sentinelserver.Credential) sentinelserver.Credential {
	if c.AccessToken == "" {
		return c
	}
	extra, ok := sentinelserver.ParseCredential(c.AccessToken)
	if !ok {
		return c
	}
	if extra.AccessToken != "" {
		c.AccessToken = extra.AccessToken
	}
	if c.SessionToken == "" {
		c.SessionToken = extra.SessionToken
	}
	if c.RefreshToken == "" {
		c.RefreshToken = extra.RefreshToken
	}
	if c.ExpiresAt.IsZero() {
		c.ExpiresAt = extra.ExpiresAt
	}
	return c
}

func (s *ChatGPTAccounts) upsert(ctx context.Context, cred sentinelserver.Credential, persist bool) error {
	cred = expandChatGPTCredential(cred)
	if cred.ID == "" {
		return fmt.Errorf("%s: credential missing id", ProviderKey)
	}
	if cred.AccessToken == "" && cred.SessionToken == "" && cred.RefreshToken == "" {
		return nil
	}
	now := time.Now().UTC()
	auth := newChatGPTAuth(cred.ID, cred, now)
	bindChatGPTAuthFile(auth, cred.ID, s.authDir)
	registerCtx := ctx
	if !persist {
		registerCtx = coreauth.WithSkipPersist(ctx)
	}
	var existing *coreauth.Auth
	for _, candidate := range s.mgr.List() {
		if candidate != nil && candidate.ID == auth.ID {
			existing = candidate
			break
		}
	}
	// 重灌已有账号只更新凭证本身：面板里配的 note / proxy_url / 停用状态等都要留下。
	carryOverUserSettings(auth, existing, chatgptManagedMetaKeys)
	// 新灌的号默认带 chatgpt-web/ 前缀；重灌已有账号时沿用它现有的前缀（用户可能在面板里改过）。
	setModelPrefix(auth, importModelPrefix(existing, defaultChatGPTModelPrefix))
	if existing != nil {
		if _, err := s.mgr.Update(registerCtx, auth); err != nil {
			return err
		}
		return s.ensurePersisted(auth, persist)
	}
	if _, err := s.mgr.Register(registerCtx, auth); err != nil {
		return err
	}
	return s.ensurePersisted(auth, persist)
}

func (s *ChatGPTAccounts) ensurePersisted(auth *coreauth.Auth, persist bool) error {
	if !persist || s.authDir == "" || auth == nil {
		return nil
	}
	path := strings.TrimSpace(auth.Attributes[coreauth.AttributePath])
	if path == "" {
		path = filepath.Join(s.authDir, chatgptWebAuthFileName(strings.TrimPrefix(auth.ID, ProviderKey+"-")))
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("persist auth-dir %s: %w", path, err)
	}
	return nil
}

func (s *ChatGPTAccounts) registerAuthDir(ctx context.Context) (int, error) {
	if s.authDir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(s.authDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read auth-dir: %w", err)
	}
	n := 0
	now := time.Now().UTC()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		data, errRead := os.ReadFile(filepath.Join(s.authDir, name))
		if errRead != nil {
			continue
		}
		meta, ok := chatgptAuthFileMeta(data)
		if !ok {
			continue
		}
		cred, ok := credentialFromMeta(meta)
		if !ok {
			continue
		}
		if cred.ID == "" {
			cred.ID = strings.TrimSuffix(name, filepath.Ext(name))
		}
		auth := newChatGPTAuth(cred.ID, cred, now)
		bindChatGPTAuthPath(auth, s.authDir, name)
		// 以文件为准：有 prefix 就带上，老文件没有就不补（见 defaultChatGPTModelPrefix 处的说明）。
		setModelPrefix(auth, modelPrefixFromMeta(meta))
		if _, err = s.mgr.Register(coreauth.WithSkipPersist(ctx), auth); err != nil {
			return n, fmt.Errorf("register auth %s: %w", auth.ID, err)
		}
		n++
	}
	return n, nil
}

func (s *ChatGPTAccounts) migrateChatGPTFile(ctx context.Context, path string) (int, error) {
	creds, err := sentinelserver.LoadChatGPTCredentials(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read chatgpt file: %w", err)
	}
	n := 0
	for _, cred := range creds {
		cred = expandChatGPTCredential(cred)
		if cred.ID == "" {
			continue
		}
		if s.authDir != "" {
			if _, statErr := os.Stat(filepath.Join(s.authDir, chatgptWebAuthFileName(cred.ID))); statErr == nil {
				continue
			}
		}
		if err = s.upsert(ctx, cred, s.authDir != ""); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func newChatGPTAuth(id string, cred sentinelserver.Credential, now time.Time) *coreauth.Auth {
	meta := map[string]any{"type": ProviderKey}
	attrs := map[string]string{}
	if cred.AccessToken != "" {
		meta["access_token"] = cred.AccessToken
		attrs["api_key"] = cred.AccessToken
	}
	if cred.RefreshToken != "" {
		meta["refresh_token"] = cred.RefreshToken
	}
	if cred.SessionToken != "" {
		meta["session_token"] = cred.SessionToken
	}
	if !cred.ExpiresAt.IsZero() {
		meta["expired"] = cred.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if cred.RefreshToken != "" || cred.SessionToken != "" {
		attrs[coreauth.AttributeAuthKind] = coreauth.AuthKindOAuth
	}
	return &coreauth.Auth{
		ID:         ProviderKey + "-" + id,
		Provider:   ProviderKey,
		Label:      ProviderKey + ":" + id,
		Status:     coreauth.StatusActive,
		Attributes: attrs,
		Metadata:   meta,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

func chatgptWebAuthFileName(accountID string) string {
	return ProviderKey + "-" + accountID + ".json"
}

func chatgptWebAuthID(fileName string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(fileName)
	}
	return fileName
}

func bindChatGPTAuthFile(auth *coreauth.Auth, accountID, authDir string) {
	if auth == nil || strings.TrimSpace(authDir) == "" || strings.TrimSpace(accountID) == "" {
		return
	}
	bindChatGPTAuthPath(auth, authDir, chatgptWebAuthFileName(accountID))
}

func bindChatGPTAuthPath(auth *coreauth.Auth, authDir, fileName string) {
	if auth == nil || strings.TrimSpace(authDir) == "" || strings.TrimSpace(fileName) == "" {
		return
	}
	path := filepath.Join(authDir, fileName)
	auth.FileName = fileName
	auth.ID = chatgptWebAuthID(fileName)
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes[coreauth.AttributePath] = path
	auth.Attributes[coreauth.AttributeSource] = path
	auth.Attributes[coreauth.AttributeSourceBackend] = coreauth.AuthSourceFile
}

func credentialFromAuthFile(data []byte) (sentinelserver.Credential, bool) {
	meta, ok := chatgptAuthFileMeta(data)
	if !ok {
		return sentinelserver.Credential{}, false
	}
	return credentialFromMeta(meta)
}

// chatgptAuthFileMeta 解析 auth-dir 文件，只接受 type=chatgpt-web 的。
func chatgptAuthFileMeta(data []byte) (map[string]any, bool) {
	var meta map[string]any
	if json.Unmarshal(data, &meta) != nil {
		return nil, false
	}
	if t, _ := meta["type"].(string); !strings.EqualFold(strings.TrimSpace(t), ProviderKey) {
		return nil, false
	}
	return meta, true
}

func credentialFromMeta(meta map[string]any) (sentinelserver.Credential, bool) {
	cred := sentinelserver.Credential{
		AccessToken:  strings.TrimSpace(metaString(meta, "access_token", "accessToken")),
		RefreshToken: strings.TrimSpace(metaString(meta, "refresh_token", "refreshToken")),
		SessionToken: strings.TrimSpace(metaString(meta, "session_token", "sessionToken")),
	}
	if exp, ok := parseMetaTime(meta["expired"], meta["expires_at"], meta["expiresAt"]); ok {
		cred.ExpiresAt = exp
	}
	return cred, cred.AccessToken != "" || cred.RefreshToken != "" || cred.SessionToken != ""
}

func removeChatGPTAuthFiles(authDir string) error {
	entries, err := os.ReadDir(authDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		path := filepath.Join(authDir, entry.Name())
		data, errRead := os.ReadFile(path)
		if errRead != nil {
			continue
		}
		if _, ok := credentialFromAuthFile(data); !ok {
			continue
		}
		if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func metaString(meta map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := meta[key].(string); ok {
			return s
		}
	}
	return ""
}

func parseMetaTime(values ...any) (time.Time, bool) {
	for _, value := range values {
		switch v := value.(type) {
		case string:
			if ts, err := time.Parse(time.RFC3339, strings.TrimSpace(v)); err == nil {
				return ts, true
			}
		case time.Time:
			if !v.IsZero() {
				return v.UTC(), true
			}
		}
	}
	return time.Time{}, false
}

// RegisterAuthsFromTokensFile is the old name of RegisterAuthsFromChatGPTFile.
func RegisterAuthsFromTokensFile(ctx context.Context, mgr *coreauth.Manager, path string) (int, error) {
	return RegisterAuthsFromChatGPTFile(ctx, mgr, path, "")
}
