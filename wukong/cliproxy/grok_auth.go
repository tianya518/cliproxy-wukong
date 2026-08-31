package cliproxy

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

	"github.com/router-for-me/CLIProxyAPI/v7/wukong/grok"
	sentinelserver "github.com/router-for-me/CLIProxyAPI/v7/wukong/server"
)

func grokCredentialFrom(auth *coreauth.Auth) (grok.Credential, error) {
	if auth == nil {
		return grok.Credential{}, requestError{fmt.Errorf("%s: no credential selected", GrokProviderKey)}
	}
	token := firstAttr(auth, "sso_token", "api_key", "access_token", "accessToken")
	if strings.TrimSpace(token) == "" {
		return grok.Credential{}, fmt.Errorf("%s: auth %q carries no sso_token", GrokProviderKey, auth.ID)
	}
	return grok.Credential{
		Name:              firstAttr(auth, "name", "label"),
		SSOToken:          token,
		UserID:            firstAttr(auth, "user_id", "userId"),
		CloudflareCookies: firstAttr(auth, "cloudflare_cookies", "cf_cookies"),
		UserAgent:         firstAttr(auth, "user_agent"),
		Tier:              firstAttr(auth, "tier"),
		Email:             firstAttr(auth, "email"),
	}, nil
}

func firstAttr(auth *coreauth.Auth, keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(auth.Attributes[key]); v != "" {
			return v
		}
		if s, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// GrokAccounts imports Grok web SSO into cliproxy's auth-dir and Manager.
// /grok/upload and startup migration both go through it.
type GrokAccounts struct {
	mgr      *coreauth.Manager
	authDir  string
	cfg      grok.Config
	onChange func()
}

// NewGrokAccounts wires Grok account import to the same auth-dir FileTokenStore as ChatGPT.
func NewGrokAccounts(mgr *coreauth.Manager, authDir string, cfg grok.Config, onChange func()) *GrokAccounts {
	return &GrokAccounts{mgr: mgr, authDir: strings.TrimSpace(authDir), cfg: cfg, onChange: onChange}
}

var _ sentinelserver.GrokAccountAdmin = (*GrokAccounts)(nil)

func (s *GrokAccounts) notify() {
	if s != nil && s.onChange != nil {
		s.onChange()
	}
}

func (s *GrokAccounts) ImportRaw(raw []byte) (int, error) {
	if s == nil || s.mgr == nil {
		return 0, fmt.Errorf("%s: account store is not configured", GrokProviderKey)
	}
	incoming, err := grok.ParseUpload(raw)
	if err != nil {
		return 0, err
	}
	if len(incoming) == 0 {
		return 0, fmt.Errorf("没有 Grok Web 账号")
	}
	return s.importCreds(context.Background(), incoming, true)
}

func (s *GrokAccounts) importCreds(ctx context.Context, accounts []grok.Credential, persist bool) (int, error) {
	added := 0
	for _, account := range accounts {
		if account.AccessToken() == "" {
			continue
		}
		if err := s.upsert(ctx, account, persist); err != nil {
			return added, err
		}
		added++
	}
	if added > 0 {
		s.notify()
	}
	return added, nil
}

func (s *GrokAccounts) Clear() error {
	if s == nil || s.mgr == nil {
		return fmt.Errorf("%s: account store is not configured", GrokProviderKey)
	}
	ctx := context.Background()
	for _, auth := range s.mgr.List() {
		if auth == nil || auth.Provider != GrokProviderKey {
			continue
		}
		if path := strings.TrimSpace(auth.Attributes[coreauth.AttributePath]); path != "" {
			_ = os.Remove(path)
		}
		s.mgr.Remove(ctx, auth.ID)
	}
	if s.authDir != "" {
		if err := removeGrokAuthFiles(s.authDir); err != nil {
			return err
		}
	}
	s.notify()
	return nil
}

func (s *GrokAccounts) Count() int {
	n := 0
	s.each(func(*coreauth.Auth) { n++ })
	return n
}

func (s *GrokAccounts) PublicAccounts() []grok.AccountPublic {
	out := make([]grok.AccountPublic, 0)
	s.each(func(auth *coreauth.Auth) {
		if cred, err := grokCredentialFrom(auth); err == nil {
			out = append(out, cred.Public())
		}
	})
	return out
}

func (s *GrokAccounts) CheckAll(ctx context.Context) []grok.AccountCheckResult {
	if s == nil || s.mgr == nil {
		return nil
	}
	var results []grok.AccountCheckResult
	s.each(func(auth *coreauth.Auth) {
		cred, err := grokCredentialFrom(auth)
		if err != nil {
			results = append(results, grok.AccountCheckResult{ID: auth.ID, Error: err.Error()})
			return
		}
		result := grok.AccountCheckResult{ID: cred.ID(), Name: cred.Name}
		identity, fetchErr := grok.NewClient(s.cfg, cred).FetchSession(ctx)
		if fetchErr != nil {
			result.Error = fetchErr.Error()
			auth.Status = coreauth.StatusError
			_, _ = s.mgr.Update(ctx, auth)
			results = append(results, result)
			return
		}
		result.Valid = true
		result.UserID = identity.UserID
		result.Email = identity.Email
		changed := false
		if identity.UserID != "" && cred.UserID != identity.UserID {
			cred.UserID = identity.UserID
			changed = true
		}
		if identity.Email != "" && cred.Email != identity.Email {
			cred.Email = identity.Email
			changed = true
		}
		auth.Status = coreauth.StatusActive
		if changed {
			applyGrokCredential(auth, cred)
		}
		_, _ = s.mgr.Update(ctx, auth)
		results = append(results, result)
	})
	return results
}

func (s *GrokAccounts) each(fn func(*coreauth.Auth)) {
	if s == nil || s.mgr == nil {
		return
	}
	for _, auth := range s.mgr.List() {
		if auth == nil || auth.Provider != GrokProviderKey {
			continue
		}
		fn(auth)
	}
}

func (s *GrokAccounts) Load(ctx context.Context, grokFile string) (int, error) {
	if s == nil || s.mgr == nil {
		return 0, fmt.Errorf("%s: account store is not configured", GrokProviderKey)
	}
	n, err := s.registerAuthDir(ctx)
	if err != nil {
		return n, err
	}
	migrated, err := s.migrateGrokFile(ctx, grokFile)
	return n + migrated, err
}

// ApplyClearanceUpdate writes refreshed Cloudflare cookies back to Manager / auth-dir.
func (s *GrokAccounts) ApplyClearanceUpdate(cred grok.Credential) {
	if s == nil || s.mgr == nil {
		return
	}
	token := cred.AccessToken()
	if token == "" {
		return
	}
	ctx := context.Background()
	s.each(func(auth *coreauth.Auth) {
		existing, err := grokCredentialFrom(auth)
		if err != nil || existing.AccessToken() != token {
			return
		}
		existing.CloudflareCookies = cred.CloudflareCookies
		existing.UserAgent = cred.UserAgent
		applyGrokCredential(auth, existing)
		_, _ = s.mgr.Update(ctx, auth)
	})
}

func (s *GrokAccounts) upsert(ctx context.Context, cred grok.Credential, persist bool) error {
	if cred.AccessToken() == "" {
		return nil
	}
	now := time.Now().UTC()
	auth := newGrokAuth(cred, now)
	bindGrokAuthFile(auth, cred.ID(), s.authDir)
	registerCtx := ctx
	if !persist {
		registerCtx = coreauth.WithSkipPersist(ctx)
	}
	for _, existing := range s.mgr.List() {
		if existing == nil || existing.ID != auth.ID {
			continue
		}
		auth.CreatedAt = existing.CreatedAt
		_, err := s.mgr.Update(registerCtx, auth)
		if err != nil {
			return err
		}
		return s.ensurePersisted(auth, persist)
	}
	_, err := s.mgr.Register(registerCtx, auth)
	if err != nil {
		return err
	}
	return s.ensurePersisted(auth, persist)
}

func (s *GrokAccounts) ensurePersisted(auth *coreauth.Auth, persist bool) error {
	if !persist || s.authDir == "" || auth == nil {
		return nil
	}
	path := strings.TrimSpace(auth.Attributes[coreauth.AttributePath])
	if path == "" {
		return fmt.Errorf("persist auth-dir: missing path for %s", auth.ID)
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("persist auth-dir %s: %w", path, err)
	}
	return nil
}

func (s *GrokAccounts) registerAuthDir(ctx context.Context) (int, error) {
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
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		name := entry.Name()
		data, errRead := os.ReadFile(filepath.Join(s.authDir, name))
		if errRead != nil {
			continue
		}
		cred, ok := grokCredentialFromAuthFile(data)
		if !ok {
			continue
		}
		auth := newGrokAuth(cred, now)
		bindGrokAuthPath(auth, s.authDir, name)
		if _, err = s.mgr.Register(coreauth.WithSkipPersist(ctx), auth); err != nil {
			return n, fmt.Errorf("register auth %s: %w", auth.ID, err)
		}
		n++
	}
	return n, nil
}

func (s *GrokAccounts) migrateGrokFile(ctx context.Context, path string) (int, error) {
	accounts, err := grok.LoadCredentialsFileOptional(path)
	if err != nil {
		return 0, fmt.Errorf("read grok file: %w", err)
	}
	n := 0
	for _, cred := range accounts {
		if cred.AccessToken() == "" {
			continue
		}
		if s.authDir != "" {
			if _, statErr := os.Stat(filepath.Join(s.authDir, grokWebAuthFileName(cred.ID()))); statErr == nil {
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

func newGrokAuth(cred grok.Credential, now time.Time) *coreauth.Auth {
	id := cred.ID()
	token := cred.AccessToken()
	meta := map[string]any{"type": GrokProviderKey, "sso_token": token}
	attrs := map[string]string{"sso_token": token, "api_key": token}
	setGrokField := func(key, value string) {
		if value == "" {
			return
		}
		meta[key] = value
		attrs[key] = value
	}
	setGrokField("name", strings.TrimSpace(cred.Name))
	setGrokField("user_id", cred.UserID)
	setGrokField("email", cred.Email)
	setGrokField("cloudflare_cookies", cred.CloudflareCookies)
	setGrokField("user_agent", cred.UserAgent)
	setGrokField("tier", string(cred.WebTier()))
	return &coreauth.Auth{
		ID:         GrokProviderKey + "-" + id,
		Provider:   GrokProviderKey,
		Label:      GrokProviderKey + ":" + id,
		Status:     coreauth.StatusActive,
		Attributes: attrs,
		Metadata:   meta,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

func applyGrokCredential(auth *coreauth.Auth, cred grok.Credential) {
	if auth == nil {
		return
	}
	next := newGrokAuth(cred, time.Now().UTC())
	auth.Metadata = next.Metadata
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	for k, v := range next.Attributes {
		auth.Attributes[k] = v
	}
	auth.UpdatedAt = next.UpdatedAt
	auth.Status = coreauth.StatusActive
}

func grokWebAuthFileName(accountID string) string {
	return GrokProviderKey + "-" + accountID + ".json"
}

func grokWebAuthID(fileName string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(fileName)
	}
	return fileName
}

func bindGrokAuthFile(auth *coreauth.Auth, accountID, authDir string) {
	if auth == nil || strings.TrimSpace(authDir) == "" || strings.TrimSpace(accountID) == "" {
		return
	}
	bindGrokAuthPath(auth, authDir, grokWebAuthFileName(accountID))
}

func bindGrokAuthPath(auth *coreauth.Auth, authDir, fileName string) {
	if auth == nil || strings.TrimSpace(authDir) == "" || strings.TrimSpace(fileName) == "" {
		return
	}
	path := filepath.Join(authDir, fileName)
	auth.FileName = fileName
	auth.ID = grokWebAuthID(fileName)
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes[coreauth.AttributePath] = path
	auth.Attributes[coreauth.AttributeSource] = path
	auth.Attributes[coreauth.AttributeSourceBackend] = coreauth.AuthSourceFile
}

func grokCredentialFromAuthFile(data []byte) (grok.Credential, bool) {
	var meta map[string]any
	if json.Unmarshal(data, &meta) != nil {
		return grok.Credential{}, false
	}
	if t, _ := meta["type"].(string); !strings.EqualFold(strings.TrimSpace(t), GrokProviderKey) {
		return grok.Credential{}, false
	}
	cred := grok.Credential{
		Name:              strings.TrimSpace(metaString(meta, "name")),
		SSOToken:          strings.TrimSpace(metaString(meta, "sso_token", "token")),
		UserID:            strings.TrimSpace(metaString(meta, "user_id", "userId")),
		Email:             strings.TrimSpace(metaString(meta, "email")),
		CloudflareCookies: strings.TrimSpace(metaString(meta, "cloudflare_cookies", "cf_cookies")),
		UserAgent:         strings.TrimSpace(metaString(meta, "user_agent")),
		Tier:              strings.TrimSpace(metaString(meta, "tier")),
	}
	return cred, cred.AccessToken() != ""
}

func removeGrokAuthFiles(authDir string) error {
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
		if _, ok := grokCredentialFromAuthFile(data); !ok {
			continue
		}
		if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func RegisterAuthsFromGrokFile(ctx context.Context, mgr *coreauth.Manager, path string) (int, error) {
	return NewGrokAccounts(mgr, "", grok.Config{}, nil).Load(ctx, path)
}

func RegisterGrokAuths(ctx context.Context, mgr *coreauth.Manager, accounts []grok.Credential) (int, error) {
	return NewGrokAccounts(mgr, "", grok.Config{}, nil).importCreds(ctx, accounts, false)
}

// ReplaceGrokAuths replaces grok-web auths and leaves chatgpt-web untouched.
func ReplaceGrokAuths(ctx context.Context, mgr *coreauth.Manager, accounts []grok.Credential) error {
	if mgr == nil {
		return nil
	}
	s := NewGrokAccounts(mgr, "", grok.Config{}, nil)
	keep := make(map[string]struct{}, len(accounts))
	for _, account := range accounts {
		keep[GrokProviderKey+"-"+account.ID()] = struct{}{}
	}
	for _, auth := range mgr.List() {
		if auth == nil || auth.Provider != GrokProviderKey {
			continue
		}
		if _, ok := keep[auth.ID]; ok {
			continue
		}
		mgr.Remove(ctx, auth.ID)
	}
	_, err := s.importCreds(ctx, accounts, false)
	return err
}
