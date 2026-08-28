package grok

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	maxImportAccounts = 10000
	maxSSOTokenBytes  = 16 << 10
)

type Credential struct {
	Name              string     `json:"name,omitempty"`
	Email             string     `json:"email,omitempty"`
	UserID            string     `json:"user_id,omitempty"`
	SSOToken          string     `json:"sso_token,omitempty"`
	Token             string     `json:"token,omitempty"`
	Tier              string     `json:"tier,omitempty"`
	CloudflareCookies string     `json:"cloudflare_cookies,omitempty"`
	UserAgent         string     `json:"user_agent,omitempty"`
	NSFWEnabledAt     *time.Time `json:"nsfw_enabled_at,omitempty"`
	TOSAcceptedAt     *time.Time `json:"tos_accepted_at,omitempty"`
	TOSVersion        int        `json:"tos_version,omitempty"`
	BirthDateSetAt    *time.Time `json:"birth_date_set_at,omitempty"`
}

type importDocument struct {
	Provider string       `json:"provider"`
	Accounts []Credential `json:"accounts"`
}

func (c Credential) AccessToken() string {
	return sanitizeSSOToken(firstNonEmpty(c.SSOToken, c.Token))
}

func (c Credential) WebTier() Tier {
	tier := Tier(strings.ToLower(strings.TrimSpace(c.Tier)))
	switch tier {
	case TierBasic, TierSuper, TierHeavy, TierAuto:
		return tier
	default:
		return TierAuto
	}
}

func (c Credential) ID() string {
	token := c.AccessToken()
	sum := sha256.Sum256([]byte(token))
	if name := strings.TrimSpace(c.Name); name != "" {
		return name
	}
	return hex.EncodeToString(sum[:6])
}

func ParseCredentials(data []byte) ([]Credential, error) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, fmt.Errorf("账号文件中没有 Grok Web 账号")
	}
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return parsePlainTextCredentials(trimmed)
	}
	var entries []Credential
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal(data, &entries); err != nil {
			return nil, fmt.Errorf("解析 Grok Web 账号 JSON: %w", err)
		}
	} else {
		var doc importDocument
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("解析 Grok Web 账号 JSON: %w", err)
		}
		if doc.Provider != "" && doc.Provider != "grok_web" && doc.Provider != "grok-web" {
			return nil, fmt.Errorf("账号文件 provider=%q 不是 grok_web", doc.Provider)
		}
		entries = doc.Accounts
	}
	return normalizeCredentials(entries)
}

func LoadCredentialsFile(path string) ([]Credential, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseCredentials(raw)
}

// LoadCredentialsFileOptional 文件不存在或为空时返回空列表，方便先起服务再灌号。
func LoadCredentialsFileOptional(path string) ([]Credential, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	return ParseCredentials(raw)
}

func SaveCredentialsFile(path string, accounts []Credential) error {
	if accounts == nil {
		accounts = []Credential{}
	}
	clean := make([]Credential, 0, len(accounts))
	for _, account := range accounts {
		account.SSOToken = account.AccessToken()
		account.Token = ""
		clean = append(clean, account)
	}
	raw, err := json.MarshalIndent(importDocument{Provider: "grok_web", Accounts: clean}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

// Public 是对外展示用的账号摘要，不含 SSO。
type AccountPublic struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	Email         string `json:"email,omitempty"`
	UserID        string `json:"user_id,omitempty"`
	Tier          string `json:"tier,omitempty"`
	HasSSO        bool   `json:"has_sso"`
	HasCloudflare bool   `json:"has_cloudflare"`
}

func (c Credential) Public() AccountPublic {
	return AccountPublic{
		ID:            c.ID(),
		Name:          strings.TrimSpace(c.Name),
		Email:         strings.TrimSpace(c.Email),
		UserID:        strings.TrimSpace(c.UserID),
		Tier:          string(c.WebTier()),
		HasSSO:        c.AccessToken() != "",
		HasCloudflare: strings.TrimSpace(c.CloudflareCookies) != "",
	}
}

// ParseUpload 解析 /grok/upload 的 body：grok.json 整段、账号数组、单条 JSON，或纯 SSO 文本。
func ParseUpload(raw []byte) ([]Credential, error) {
	raw = bytes.TrimPrefix(bytes.TrimSpace(raw), []byte{0xef, 0xbb, 0xbf})
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("没有 Grok Web 账号")
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, fmt.Errorf("解析 Grok Web 账号 JSON: %w", err)
		}
		if _, ok := probe["provider"]; ok {
			return ParseCredentials(raw)
		}
		if acc, ok := probe["accounts"]; ok {
			acc = bytes.TrimSpace(acc)
			if len(acc) > 0 && (acc[0] == '[' || acc[0] == '{') {
				return ParseCredentials(raw)
			}
			var asString string
			if json.Unmarshal(acc, &asString) == nil && strings.TrimSpace(asString) != "" {
				creds, err := ParseCredentials([]byte(asString))
				if err != nil {
					return nil, err
				}
				return applyUploadMeta(creds, probe), nil
			}
		}
		if cred, ok := singleUploadCredential(probe); ok {
			return normalizeCredentials([]Credential{cred})
		}
	}
	return ParseCredentials(raw)
}

func singleUploadCredential(probe map[string]json.RawMessage) (Credential, bool) {
	for _, key := range []string{"sso", "sso_token", "text"} {
		value := jsonString(probe, key)
		if value == "" {
			continue
		}
		return Credential{
			SSOToken:          value,
			Name:              jsonString(probe, "name"),
			Email:             jsonString(probe, "email"),
			UserID:            jsonString(probe, "user_id"),
			CloudflareCookies: jsonString(probe, "cloudflare_cookies"),
			UserAgent:         jsonString(probe, "user_agent"),
			Tier:              jsonString(probe, "tier"),
		}, true
	}
	return Credential{}, false
}

func applyUploadMeta(creds []Credential, probe map[string]json.RawMessage) []Credential {
	if len(creds) != 1 {
		return creds
	}
	if name := jsonString(probe, "name"); name != "" {
		creds[0].Name = name
	}
	if email := jsonString(probe, "email"); email != "" {
		creds[0].Email = email
	}
	if userID := jsonString(probe, "user_id"); userID != "" {
		creds[0].UserID = userID
	}
	if cookies := jsonString(probe, "cloudflare_cookies"); cookies != "" {
		creds[0].CloudflareCookies = cookies
	}
	if userAgent := jsonString(probe, "user_agent"); userAgent != "" {
		creds[0].UserAgent = userAgent
	}
	if tier := jsonString(probe, "tier"); tier != "" {
		creds[0].Tier = tier
	}
	return creds
}

func jsonString(probe map[string]json.RawMessage, key string) string {
	raw, ok := probe[key]
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func normalizeCredentials(entries []Credential) ([]Credential, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	if len(entries) > maxImportAccounts {
		return nil, fmt.Errorf("Grok Web 账号超过 %d 个", maxImportAccounts)
	}
	seen := make(map[string]struct{}, len(entries))
	out := make([]Credential, 0, len(entries))
	for index, entry := range entries {
		token := entry.AccessToken()
		if token == "" {
			return nil, fmt.Errorf("第 %d 个账号缺少 sso_token", index+1)
		}
		if len(token) > maxSSOTokenBytes {
			return nil, fmt.Errorf("第 %d 个账号的 sso_token 超过 16 KiB", index+1)
		}
		if _, exists := seen[token]; exists {
			continue
		}
		seen[token] = struct{}{}
		tier := entry.WebTier()
		entry.SSOToken = token
		entry.Token = ""
		entry.Tier = string(tier)
		if strings.TrimSpace(entry.Name) == "" {
			sum := sha256.Sum256([]byte(token))
			entry.Name = "Grok Web " + hex.EncodeToString(sum[:4])
		}
		out = append(out, entry)
	}
	return out, nil
}

func parsePlainTextCredentials(value string) ([]Credential, error) {
	lines := strings.Split(value, "\n")
	entries := make([]Credential, 0, len(lines))
	for _, line := range lines {
		token := sanitizeSSOToken(line)
		if token == "" {
			continue
		}
		entries = append(entries, Credential{SSOToken: token, Tier: string(TierAuto)})
	}
	return normalizeCredentials(entries)
}

func sanitizeSSOToken(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "sso=") {
		value = strings.TrimSpace(value[len("sso="):])
	}
	if token, _, found := strings.Cut(value, ";"); found {
		value = token
	}
	return strings.TrimSpace(strings.NewReplacer("\r", "", "\n", "", "\x00", "").Replace(value))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
