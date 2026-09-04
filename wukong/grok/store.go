package grok

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
)

// AccountStore 管理 grok.json：添加、清空、探测会话，并可选通知 cliproxy 热更新。
type AccountStore struct {
	path     string
	cfg      Config
	mu       sync.Mutex
	accounts []Credential
	onChange func([]Credential)
}

func NewAccountStore(path string, cfg Config) *AccountStore {
	s := &AccountStore{path: path, cfg: cfg.normalized()}
	prev := s.cfg.OnClearanceUpdate
	s.cfg.OnClearanceUpdate = func(cred Credential) {
		s.ApplyClearanceUpdate(cred)
		if prev != nil {
			prev(cred)
		}
	}
	accounts, err := LoadCredentialsFileOptional(path)
	if err != nil {
		log.Printf("[grok] 读取 %s 失败: %v", path, err)
	} else {
		s.accounts = accounts
		if len(accounts) > 0 {
			log.Printf("[grok] 已加载 %d 个账号 ← %s", len(accounts), path)
		}
	}
	return s
}

func (s *AccountStore) SetOnChange(fn func([]Credential)) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

func (s *AccountStore) ClientConfig() Config {
	if s == nil {
		return Config{}
	}
	return s.cfg
}

func (s *AccountStore) ApplyClearanceUpdate(cred Credential) {
	if s == nil {
		return
	}
	token := cred.AccessToken()
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, account := range s.accounts {
		if account.AccessToken() != token {
			continue
		}
		s.accounts[i].CloudflareCookies = cred.CloudflareCookies
		s.accounts[i].UserAgent = cred.UserAgent
		_ = s.persistLocked()
		return
	}
}

func (s *AccountStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *AccountStore) Count() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.accounts)
}

func (s *AccountStore) Snapshot() []Credential {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Credential(nil), s.accounts...)
}

func (s *AccountStore) PublicAccounts() []AccountPublic {
	accounts := s.Snapshot()
	out := make([]AccountPublic, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, account.Public())
	}
	return out
}

func (s *AccountStore) ImportRaw(raw []byte) (int, error) {
	incoming, err := ParseUpload(raw)
	if err != nil {
		return 0, err
	}
	if len(incoming) == 0 {
		return 0, fmt.Errorf("没有 Grok Web 账号")
	}
	return s.Add(incoming...)
}

func (s *AccountStore) Add(incoming ...Credential) (int, error) {
	if s == nil {
		return 0, nil
	}
	normalized, err := normalizeCredentials(incoming)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	added := 0
	for _, account := range normalized {
		if merged, ok := mergeInto(s.accounts, account); ok {
			s.accounts = merged
			added++
		} else {
			s.accounts = merged
		}
	}
	err = s.persistLocked()
	snap := append([]Credential(nil), s.accounts...)
	fn := s.onChange
	s.mu.Unlock()
	if fn != nil {
		fn(snap)
	}
	return added, err
}

func (s *AccountStore) Clear() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.accounts = nil
	err := s.persistLocked()
	fn := s.onChange
	s.mu.Unlock()
	if fn != nil {
		fn(nil)
	}
	return err
}

type AccountCheckResult struct {
	ID           string           `json:"id"`
	Name         string           `json:"name,omitempty"`
	Valid        bool             `json:"valid"`
	UserID       string           `json:"user_id,omitempty"`
	Email        string           `json:"email,omitempty"`
	Tier         Tier             `json:"tier,omitempty"`
	Windows      []QuotaWindow    `json:"windows,omitempty"`
	Billing      *BillingSnapshot `json:"billing,omitempty"`
	BillingError string           `json:"billing_error,omitempty"`
	QuotaError   string           `json:"quota_error,omitempty"`
	Error        string           `json:"error,omitempty"`
}

// AttachQuota 在会话有效时补一次额度。额度失败不影响 valid 判定。
func (r *AccountCheckResult) AttachQuota(ctx context.Context, cfg Config, cred Credential) {
	quota := QuotaFor(ctx, cfg, cred)
	r.Tier = quota.Tier
	r.Windows = quota.Windows
	r.Billing = quota.Billing
	r.BillingError = quota.BillingError
	r.QuotaError = quota.Error
}

// Quota 拉额度。id 为空拉全部；否则只拉 ID 匹配的那个账号。
func (s *AccountStore) Quota(ctx context.Context, id string) []AccountQuotaResult {
	if s == nil {
		return nil
	}
	accounts := s.Snapshot()
	results := make([]AccountQuotaResult, 0, len(accounts))
	for _, account := range accounts {
		if id != "" && !strings.EqualFold(account.ID(), id) {
			continue
		}
		results = append(results, QuotaFor(ctx, s.cfg, account))
	}
	return results
}

func (s *AccountStore) CheckAll(ctx context.Context, withQuota bool) []AccountCheckResult {
	if s == nil {
		return nil
	}
	accounts := s.Snapshot()
	results := make([]AccountCheckResult, 0, len(accounts))
	updated := make([]Credential, 0, len(accounts))
	changed := false
	for _, account := range accounts {
		result := AccountCheckResult{ID: account.ID(), Name: account.Name}
		client := NewClient(s.cfg, account)
		identity, err := client.FetchSession(ctx)
		if err != nil {
			result.Error = err.Error()
			updated = append(updated, account)
			results = append(results, result)
			continue
		}
		result.Valid = true
		result.UserID = identity.UserID
		result.Email = identity.Email
		if identity.UserID != "" && account.UserID != identity.UserID {
			account.UserID = identity.UserID
			changed = true
		}
		if identity.Email != "" && account.Email != identity.Email {
			account.Email = identity.Email
			changed = true
		}
		if withQuota {
			result.AttachQuota(ctx, s.cfg, account)
		}
		updated = append(updated, account)
		results = append(results, result)
	}
	if changed {
		s.mu.Lock()
		s.accounts = updated
		_ = s.persistLocked()
		fn := s.onChange
		snap := append([]Credential(nil), s.accounts...)
		s.mu.Unlock()
		if fn != nil {
			fn(snap)
		}
	}
	return results
}

func (s *AccountStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	if err := SaveCredentialsFile(s.path, s.accounts); err != nil {
		log.Printf("[grok] 保存 %s 失败: %v", s.path, err)
		return err
	}
	return nil
}

// mergeInto 按 SSO 去重。新账号返回 true；已存在则合并元数据并返回 false。
func mergeInto(existing []Credential, incoming Credential) ([]Credential, bool) {
	token := incoming.AccessToken()
	for i, account := range existing {
		if account.AccessToken() != token {
			continue
		}
		existing[i] = mergeAccount(account, incoming)
		return existing, false
	}
	return append(existing, incoming), true
}

func mergeAccount(old, incoming Credential) Credential {
	if incoming.Name != "" {
		old.Name = incoming.Name
	}
	if incoming.Email != "" {
		old.Email = incoming.Email
	}
	if incoming.UserID != "" {
		old.UserID = incoming.UserID
	}
	if incoming.CloudflareCookies != "" {
		old.CloudflareCookies = incoming.CloudflareCookies
	}
	if incoming.UserAgent != "" {
		old.UserAgent = incoming.UserAgent
	}
	if incoming.Tier != "" {
		old.Tier = string(incoming.WebTier())
	}
	old.SSOToken = incoming.AccessToken()
	old.Token = ""
	if incoming.NSFWEnabledAt != nil {
		old.NSFWEnabledAt = incoming.NSFWEnabledAt
	}
	if incoming.TOSAcceptedAt != nil {
		old.TOSAcceptedAt = incoming.TOSAcceptedAt
	}
	if incoming.TOSVersion != 0 {
		old.TOSVersion = incoming.TOSVersion
	}
	if incoming.BirthDateSetAt != nil {
		old.BirthDateSetAt = incoming.BirthDateSetAt
	}
	return old
}
