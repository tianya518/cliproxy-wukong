package cliproxy

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func registerNativeProviderForTest(t *testing.T, provider string, models func() []*ModelInfo) {
	t.Helper()
	nativeProvidersMu.RLock()
	previous, existed := nativeProviders[provider]
	nativeProvidersMu.RUnlock()
	RegisterNativeProvider(&NativeProvider{
		Key: provider, Executor: mockBatchTestExecutor{provider: provider}, Models: models,
	})
	t.Cleanup(func() {
		nativeProvidersMu.Lock()
		defer nativeProvidersMu.Unlock()
		if existed {
			nativeProviders[provider] = previous
		} else {
			delete(nativeProviders, provider)
		}
	})
}

func TestNativeProviderModelRegistration(t *testing.T) {
	for _, provider := range []string{"chatgpt-web", "grok-web"} {
		t.Run(provider, func(t *testing.T) {
			models := []*ModelInfo{{ID: "model-live", MetadataModelID: "upstream-live"}}
			calls := 0
			registerNativeProviderForTest(t, provider, func() []*ModelInfo {
				calls++
				return models
			})
			service := &Service{
				cfg:         &config.Config{SDKConfig: config.SDKConfig{ForceModelPrefix: true}},
				coreManager: coreauth.NewManager(nil, nil, nil),
			}
			auth, errRegister := service.coreManager.Register(t.Context(), &coreauth.Auth{
				ID: t.Name(), Provider: provider, Prefix: "account", Status: coreauth.StatusActive,
			})
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			reg := GlobalModelRegistry()
			t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
			if !service.registerModelsForAuth(t.Context(), auth) {
				t.Fatal("current native catalog was rejected")
			}
			registered := reg.GetModelsForClient(auth.ID)
			if len(registered) != 1 || registered[0].ID != "account/model-live" || registered[0].MetadataModelID != "upstream-live" {
				t.Fatalf("registered native catalog = %v, want one prefixed model with its upstream metadata", registered)
			}
			if models[0].ID != "model-live" {
				t.Fatal("registration mutated the provider's source catalog")
			}
			epoch := reg.ClientRegistrationEpoch(auth.ID)
			models = nil
			if !service.registerModelsForAuth(t.Context(), auth) {
				t.Fatal("current empty native catalog was rejected")
			}
			if got := reg.GetModelsForClient(auth.ID); len(got) != 0 {
				t.Fatalf("empty native catalog left models registered: %v", got)
			}
			if got := reg.ClientRegistrationEpoch(auth.ID); got != epoch+1 {
				t.Fatalf("empty catalog epoch = %d, want %d", got, epoch+1)
			}
			if calls != 2 {
				t.Fatalf("native catalog calls = %d, want 2", calls)
			}
		})
	}
}

func TestNativeProviderNilCatalogUnregistersModels(t *testing.T) {
	for _, provider := range []string{"chatgpt-web", "grok-web"} {
		t.Run(provider, func(t *testing.T) {
			registerNativeProviderForTest(t, provider, nil)
			service := &Service{cfg: &config.Config{}}
			auth := &coreauth.Auth{ID: t.Name(), Provider: provider}
			reg := GlobalModelRegistry()
			reg.RegisterClient(auth.ID, provider, []*ModelInfo{{ID: "previous-model"}})
			t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
			if !service.registerModelsForAuth(t.Context(), auth) {
				t.Fatal("nil native catalog was rejected")
			}
			if got := reg.GetModelsForClient(auth.ID); len(got) != 0 {
				t.Fatalf("nil native catalog left models registered: %v", got)
			}
		})
	}
}

func TestNativeProviderModelRegistrationUsesConfigSnapshot(t *testing.T) {
	for _, provider := range []string{"chatgpt-web", "grok-web"} {
		for _, tc := range []struct {
			name         string
			snapshot     *config.Config
			currentForce bool
			wantBare     bool
		}{
			{name: "optional prefix", snapshot: &config.Config{}, currentForce: true, wantBare: true},
			{name: "forced prefix", snapshot: &config.Config{SDKConfig: config.SDKConfig{ForceModelPrefix: true}}, wantBare: false},
			{name: "nil config", currentForce: true, wantBare: true},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				registerNativeProviderForTest(t, provider, func() []*ModelInfo {
					return []*ModelInfo{{ID: "model-live"}}
				})
				service := &Service{cfg: &config.Config{SDKConfig: config.SDKConfig{ForceModelPrefix: tc.currentForce}}}
				auth := &coreauth.Auth{ID: t.Name(), Provider: provider, Prefix: "account"}
				reg := GlobalModelRegistry()
				t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
				if !service.registerModelsForAuthWithConfigAndCache(t.Context(), tc.snapshot, auth, nil) {
					t.Fatal("valid native registration was rejected")
				}
				if !reg.ClientSupportsModel(auth.ID, "account/model-live") || reg.ClientSupportsModel(auth.ID, "model-live") != tc.wantBare {
					t.Fatalf("native registration ignored config snapshot: prefixed=%t, bare=%t; want prefixed=true, bare=%t", reg.ClientSupportsModel(auth.ID, "account/model-live"), reg.ClientSupportsModel(auth.ID, "model-live"), tc.wantBare)
				}
			})
		}
	}
}

func TestNativeProviderModelRegistrationRejectsStalePublication(t *testing.T) {
	for _, provider := range []string{"chatgpt-web", "grok-web"} {
		for _, reason := range []string{"auth update", "auth removed", "auth recreated", "registry epoch", "config exclusions"} {
			for _, empty := range []bool{false, true} {
				operation := "publish"
				if empty {
					operation = "unregister"
				}
				t.Run(provider+"/"+reason+"/"+operation, func(t *testing.T) {
					cfg := &config.Config{OAuthExcludedModels: map[string][]string{provider: {"blocked-old"}}}
					manager := coreauth.NewManager(nil, nil, nil)
					service := &Service{cfg: cfg, coreManager: manager}
					auth, errRegister := manager.Register(t.Context(), &coreauth.Auth{
						ID: t.Name(), Provider: provider, Prefix: "account", Status: coreauth.StatusActive,
						Attributes: map[string]string{"auth_kind": "oauth"},
					})
					if errRegister != nil {
						t.Fatal(errRegister)
					}
					reg := GlobalModelRegistry()
					keptModel := "previous-model"
					reg.RegisterClient(auth.ID, provider, []*ModelInfo{{ID: keptModel}})
					t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
					expectedEpoch := reg.ClientRegistrationEpoch(auth.ID)
					calls := 0
					registerNativeProviderForTest(t, provider, func() []*ModelInfo {
						calls++
						if calls == 1 {
							// Each mutation happens after the caller captured its publication fence.
							switch reason {
							case "auth update":
								current := auth.Clone()
								current.Prefix = "new-account"
								if _, errUpdate := manager.Update(t.Context(), current); errUpdate != nil {
									t.Fatal(errUpdate)
								}
							case "auth removed":
								manager.Remove(t.Context(), auth.ID)
							case "auth recreated":
								manager.Remove(t.Context(), auth.ID)
								fresh := auth.Clone()
								fresh.RegistrationEpoch = 0
								fresh.Generation = 0
								recreated, errRecreate := manager.Register(t.Context(), fresh)
								if errRecreate != nil {
									t.Fatal(errRecreate)
								}
								if recreated.RegistrationEpoch == auth.RegistrationEpoch {
									t.Fatal("recreated auth did not advance its registration identity")
								}
							case "registry epoch":
								keptModel = "newer-model"
								reg.RegisterClient(auth.ID, provider, []*ModelInfo{{ID: keptModel}})
								expectedEpoch = reg.ClientRegistrationEpoch(auth.ID)
							case "config exclusions":
								next := &config.Config{OAuthExcludedModels: map[string][]string{provider: {"blocked-new"}}}
								if commit := service.commitConfigUpdate(next); commit.cfg != next {
									t.Fatal("config update was rejected")
								}
							}
						}
						if empty {
							return nil
						}
						return []*ModelInfo{{ID: "stale-model"}}
					})
					if service.registerModelsForAuth(t.Context(), auth) {
						t.Fatal("native registration did not propagate the rejected publication fence")
					}
					if calls != 1 {
						t.Fatalf("native catalog calls = %d, want 1", calls)
					}
					assertPreserved := func() {
						t.Helper()
						if got := reg.ClientRegistrationEpoch(auth.ID); got != expectedEpoch {
							t.Fatalf("stale registration advanced registry epoch from %d to %d", expectedEpoch, got)
						}
						if models := reg.GetModelsForClient(auth.ID); len(models) != 1 || models[0].ID != keptModel {
							t.Fatalf("stale registration changed current catalog: %v, want only %q", models, keptModel)
						}
					}
					assertPreserved()
					if reason == "config exclusions" {
						if service.registerModelsForAuthWithConfigAndCache(t.Context(), cfg, auth, nil) {
							t.Fatal("an explicitly stale config snapshot was accepted")
						}
						assertPreserved()
					}
				})
			}
		}
	}
}
