package cliproxy

import (
	"context"
	"fmt"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"

	"github.com/router-for-me/CLIProxyAPI/v7/wukong/grok"
)

func grokCredentialFrom(auth *coreauth.Auth) (grok.Credential, error) {
	if auth == nil {
		return grok.Credential{}, requestError{fmt.Errorf("%s: no credential selected", GrokProviderKey)}
	}
	token := firstAttr(auth, "sso_token", "api_key", "access_token", "accessToken")
	if token == "" {
		if s, ok := auth.Metadata["sso_token"].(string); ok {
			token = s
		}
	}
	if strings.TrimSpace(token) == "" {
		return grok.Credential{}, fmt.Errorf("%s: auth %q carries no sso_token", GrokProviderKey, auth.ID)
	}
	cred := grok.Credential{
		Name:              auth.Label,
		SSOToken:          token,
		UserID:            firstAttr(auth, "user_id", "userId"),
		CloudflareCookies: firstAttr(auth, "cloudflare_cookies", "cf_cookies"),
		UserAgent:         firstAttr(auth, "user_agent"),
		Tier:              firstAttr(auth, "tier"),
		Email:             firstAttr(auth, "email"),
	}
	return cred, nil
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

func RegisterAuthsFromGrokFile(ctx context.Context, mgr *coreauth.Manager, path string) (int, error) {
	accounts, err := grok.LoadCredentialsFileOptional(path)
	if err != nil {
		return 0, fmt.Errorf("read grok file: %w", err)
	}
	return RegisterGrokAuths(ctx, mgr, accounts)
}

func RegisterGrokAuths(ctx context.Context, mgr *coreauth.Manager, accounts []grok.Credential) (int, error) {
	n := 0
	now := time.Now().UTC()
	for _, account := range accounts {
		id := account.ID()
		auth := &coreauth.Auth{
			ID:       GrokProviderKey + "-" + id,
			Provider: GrokProviderKey,
			Label:    GrokProviderKey + ":" + id,
			Status:   coreauth.StatusActive,
			Attributes: map[string]string{
				"sso_token":          account.AccessToken(),
				"user_id":            account.UserID,
				"cloudflare_cookies": account.CloudflareCookies,
				"user_agent":         account.UserAgent,
				"tier":               string(account.WebTier()),
				"email":              account.Email,
			},
			CreatedAt: now,
			UpdatedAt: now,
		}
		if _, err := mgr.Register(ctx, auth); err != nil {
			return n, fmt.Errorf("register grok auth %s: %w", auth.ID, err)
		}
		n++
	}
	return n, nil
}

// ReplaceGrokAuths 用当前 grok.json 账号替换 cliproxy 里的 grok-web 凭证，不动 chatgpt-web。
func ReplaceGrokAuths(ctx context.Context, mgr *coreauth.Manager, accounts []grok.Credential) error {
	if mgr == nil {
		return nil
	}
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
	_, err := RegisterGrokAuths(ctx, mgr, accounts)
	return err
}
