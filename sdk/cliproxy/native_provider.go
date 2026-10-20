package cliproxy

// The wukong fork registers in-process providers here so config and auth reloads
// retain their native executors and models instead of replacing them with the
// OpenAI compatibility fallback. The service_executors.go and service_models.go
// hooks consult this registry before resolving built-in or plugin providers.

import (
	"strings"
	"sync"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// NativeProvider describes an in-process provider compiled into the binary.
type NativeProvider struct {
	// Key must match Auth.Provider and Executor.Identifier().
	Key string
	// Executor handles requests for all credentials of this provider.
	Executor coreauth.ProviderExecutor
	// Models returns the current catalog on every registration. It may be nil.
	// Call Service.RefreshNativeProviderModels to publish catalog changes immediately.
	Models func() []*ModelInfo
}

var (
	nativeProvidersMu sync.RWMutex
	nativeProviders   = map[string]*NativeProvider{}
)

// RegisterNativeProvider registers or replaces a provider, including before Build.
// An empty key or nil executor is ignored.
func RegisterNativeProvider(p *NativeProvider) {
	if p == nil {
		return
	}
	key := strings.ToLower(strings.TrimSpace(p.Key))
	if key == "" || p.Executor == nil {
		return
	}
	entry := *p
	entry.Key = key
	nativeProvidersMu.Lock()
	nativeProviders[key] = &entry
	nativeProvidersMu.Unlock()
}

func lookupNativeProvider(provider string) (*NativeProvider, bool) {
	key := strings.ToLower(strings.TrimSpace(provider))
	if key == "" {
		return nil, false
	}
	nativeProvidersMu.RLock()
	p, ok := nativeProviders[key]
	nativeProvidersMu.RUnlock()
	return p, ok
}

// registerNativeExecutorForAuth binds a native executor and reports whether it
// handled the auth, preventing the OpenAI compatibility fallback. Rebinding is
// needed only when the executor is missing or has been replaced.
func (s *Service) registerNativeExecutorForAuth(a *coreauth.Auth) bool {
	if s == nil || s.coreManager == nil || a == nil {
		return false
	}
	p, ok := lookupNativeProvider(a.Provider)
	if !ok {
		return false
	}
	if existing, has := s.coreManager.Executor(p.Key); !has || existing != p.Executor {
		s.coreManager.RegisterExecutor(p.Executor)
	}
	return true
}

// registerNativeModelsForAuthWithConfigFenced reports whether a native provider
// handled registration and whether its publication fence accepted the result.
// Empty catalogs use the same fenced removal path as built-in providers.
func (s *Service) registerNativeModelsForAuthWithConfigFenced(cfg *config.Config, a *coreauth.Auth, fence modelRegistrationFence, provider string) (bool, bool) {
	p, ok := lookupNativeProvider(provider)
	if !ok {
		return false, true
	}
	var models []*ModelInfo
	if p.Models != nil {
		models = p.Models()
	}
	// Use the captured config and fence so a slow catalog cannot replace newer state.
	models = applyModelPrefixes(models, a.Prefix, cfg != nil && cfg.ForceModelPrefix)
	return true, s.registerResolvedModelsForAuthFenced(a, fence, p.Key, models)
}

// RefreshNativeProviderModels registers the current catalog for every credential
// of a native provider so model discovery and routing update without an auth event.
func (s *Service) RefreshNativeProviderModels(providerKey string) {
	if s == nil || s.coreManager == nil {
		return
	}
	if _, ok := lookupNativeProvider(providerKey); !ok {
		return
	}
	providerKey = strings.ToLower(strings.TrimSpace(providerKey))
	for _, a := range s.coreManager.List() {
		if a == nil || !strings.EqualFold(strings.TrimSpace(a.Provider), providerKey) {
			continue
		}
		s.refreshModelRegistrationForAuth(a)
	}
}
