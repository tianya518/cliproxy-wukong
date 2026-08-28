package cliproxy

// native_provider.go —— wukong 薄 fork 补丁：进程内原生 provider 注册表。
//
// 背景：以库形式接入的自定义 provider（如网页逆向 chatgpt-web / grok-web）不被
// service 的重载路径认识。默认情况下 registerExecutorForAuth 会在 default 分支把它
// 当成 openai-compatibility 套上 OpenAICompatExecutor（随后报 missing provider
// baseURL），registerModelsForAuth 也会走到最后 UnregisterClient 把模型清掉。以往
// 只能靠一个 2 秒自愈循环去“抢回”绑定，既脆又有暴露窗口。
//
// 本文件让编译进二进制的 Go provider 走和内置 provider 完全一样的重载路径：
// service 在重绑 executor / 重注册模型时先问这张表，命中就用原生实现并早返回，
// 因此 executor 绑定与模型注册天然在 config/auths 热重载中存活，不再需要外部抢回。
//
// 这是整个薄 fork 唯一的新增文件；配套改动只有 service_executors.go 与
// service_models.go 各一行钩子（搜索 registerNativeExecutorForAuth /
// registerNativeModelsForAuth 即可定位）。升级上游时新文件不会冲突，钩子是单行
// 插入，rebase 成本极小。

import (
	"strings"
	"sync"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// NativeProvider 描述一个编译进二进制的进程内 provider。
type NativeProvider struct {
	// Key 是 provider 标识，必须与 Auth.Provider 和 Executor.Identifier() 一致。
	Key string
	// Executor 处理该 provider 所有凭证的执行（复用现成的 coreauth.ProviderExecutor）。
	Executor coreauth.ProviderExecutor
	// Models 返回该 provider 当前的模型目录。每次（重）注册都会调用，
	// 因此运行时变化的目录（如官网模型热更新）会在下一次注册时被采纳；
	// 想主动推送变化可调用 Service.RefreshNativeProviderModels。可为 nil。
	Models func() []*ModelInfo
}

var (
	nativeProvidersMu sync.RWMutex
	nativeProviders   = map[string]*NativeProvider{}
)

// RegisterNativeProvider 注册一个进程内 provider。可在 Build() 之前调用；
// 用相同 Key 再次注册会替换旧项。Key 为空或 Executor 为 nil 时忽略。
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

// registerNativeExecutorForAuth 在 auth 属于原生 provider 时把其 executor 绑上，
// 返回 true 表示已接管、调用方应早返回（不要再走 openai-compat 回退）。
// 幂等：重载多次只在绑定缺失或被换掉时重新注册。
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

// registerNativeModelsForAuth 在 auth 属于原生 provider 时注册其模型，
// 返回 true 表示已接管、调用方应早返回。空目录会走 UnregisterClient（复用内置逻辑）。
func (s *Service) registerNativeModelsForAuth(a *coreauth.Auth, provider string) bool {
	p, ok := lookupNativeProvider(provider)
	if !ok {
		return false
	}
	var models []*ModelInfo
	if p.Models != nil {
		models = p.Models()
	}
	// 与内置 provider 一致地套用前缀策略，再交给同一个注册入口。
	models = applyModelPrefixes(models, a.Prefix, s.cfg != nil && s.cfg.ForceModelPrefix)
	s.registerResolvedModelsForAuth(a, p.Key, models)
	return true
}

// RefreshNativeProviderModels 为某原生 provider 的所有凭证重新注册模型。
// 当该 provider 的模型目录在运行时变化（例如官网模型热更新）时调用，
// 使 /v1/models 与路由立即采纳新目录，而不必等下一次 auth 事件。
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
