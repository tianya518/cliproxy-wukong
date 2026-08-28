package cliproxy

// models.go —— 把 chatgpt-web / grok-web 注册为 cliproxy 的进程内原生 provider，
// 并向注册表提供 wukong 的模型目录。
//
// 注册走 fork 补丁新增的 sdkcliproxy.RegisterNativeProvider（见
// third_party/CLIProxyAPI/sdk/cliproxy/native_provider.go）。此后这两个 provider 与
// 内置 provider 走同一条重载路径：executor 绑定与模型注册在 config/auths 热重载中
// 天然存活，不再需要旧版那个每 2 秒“抢回”的自愈循环。运行时官网目录变化则由
// Service.RefreshNativeProviderModels 主动推送（见 cmd/wukong-gateway 的接线）。

import (
	sdkcliproxy "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"

	sentinel "github.com/router-for-me/CLIProxyAPI/v7/wukong/sentinel"
)

// fallbackModels 拉不到官网目录时对外暴露的最小集合。
var fallbackModels = []string{
	"gpt-5-5-thinking",
	"gpt-5-5",
	"gpt-5-3-instant",
	"o3",
	sentinel.ModelDALLE3,
}

// RegisterNativeProviders 把 chatgpt-web 与 grok-web 注册为进程内原生 provider。
// 必须在 cliproxy Builder.Build() 之前调用。Models 闭包每次（重）注册都会求值，
// 因此官网模型目录热更新后，下一次注册即采纳最新列表。
func RegisterNativeProviders(exec *Executor, grokExec *GrokExecutor) {
	sdkcliproxy.RegisterNativeProvider(&sdkcliproxy.NativeProvider{
		Key:      ProviderKey,
		Executor: exec,
		Models:   func() []*sdkcliproxy.ModelInfo { return modelInfos(catalogModelIDs()) },
	})
	sdkcliproxy.RegisterNativeProvider(&sdkcliproxy.NativeProvider{
		Key:      GrokProviderKey,
		Executor: grokExec,
		Models:   func() []*sdkcliproxy.ModelInfo { return grokModelInfos(grokModelIDs()) },
	})
}

// catalogModelIDs 生成对外暴露的模型 ID 列表。
//
// 与 wukong 自己的 /v1/models 保持同样规则：可配推理强度的 slug 额外暴露
// -standard / -extended 变体，否则标准客户端只能拿到默认档、选不了官网的「高」。
func catalogModelIDs() []string {
	cat := sentinel.CurrentModelCatalog()
	if cat == nil {
		return append([]string(nil), fallbackModels...)
	}

	seen := make(map[string]bool)
	out := make([]string, 0, len(cat.Models)+8)
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, m := range cat.Models {
		// Deep Research 和官网「工作」标签页的 -wm 模型都不走普通 chat 产品面。
		if m.HideFromChatCatalog() {
			continue
		}
		add(m.Slug)
		if m.ConfigurableThinkingEffort {
			for _, e := range m.ThinkingEfforts {
				add(m.Slug + "-" + e)
			}
		}
	}
	// 生图在后端是所有模型都挂载的 image_gen 工具而非独立模型，
	// dall-e-3 只是给标准客户端的触发名。
	add(sentinel.ModelDALLE3)
	return out
}

func modelInfos(ids []string) []*sdkcliproxy.ModelInfo {
	out := make([]*sdkcliproxy.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, &sdkcliproxy.ModelInfo{
			ID:          id,
			Object:      "model",
			Type:        ProviderKey,
			DisplayName: id,
			OwnedBy:     "openai",
		})
	}
	return out
}
