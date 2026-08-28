package sentinel

import "strings"

const (
	// ModelDALLE3 ChatGPT 网页生图使用的官方模型（picture_v2 + dall-e-3）。
	ModelDALLE3 = "dall-e-3"

	// 以下为兼容别名，均映射到 dall-e-3（勿与 OpenAI API 的 gpt-image-2 混用）。
	ModelGPTImage2         = "gpt-image-2"
	ModelGPTImage2Thinking = "gpt-image-2-thinking"
)

// ResolvedModel 将 OpenAI 兼容的 model 参数解析为 ChatGPT 后端设置。
type ResolvedModel struct {
	APIModel       string // 回显给客户端的 model
	ChatModel      string // 写入 conversation 请求体的 model（后端真实值）
	ForcePictureV2 bool   // 是否注入 system_hints picture_v2
	// ThinkingEffort 覆盖本次请求的 thinking_effort。
	// 空串表示"不注入此字段"（对应官网极速/o3 等不携带 thinking_effort 的模型）；
	// "standard" / "extended" 则写入请求体。
	// 特殊值 "none" 表示显式不发送该字段（等价空串但语义更清晰）。
	ThinkingEffort string
}

// effortAliases 请求名末尾的强度后缀 → thinking_effort 取值。
// 官网只有 standard / extended 两档，这里额外接受几个口语化写法。
var effortAliases = map[string]string{
	"extended": "extended",
	"advanced": "extended",
	"deep":     "extended",
	"high":     "extended",
	"standard": "standard",
	"balanced": "standard",
	"medium":   "standard",
}

// splitEffortSuffix 拆出请求名末尾的强度后缀，无后缀时 effort 为空。
func splitEffortSuffix(name string) (base, effort string) {
	idx := strings.LastIndex(name, "-")
	if idx <= 0 {
		return name, ""
	}
	if e, ok := effortAliases[name[idx+1:]]; ok {
		return name[:idx], e
	}
	return name, ""
}

// resolveFromCatalog 用运行时拉取的官网目录解析 model，未命中返回 false 交给静态表。
//
// 官网的「推理强度」不是单一字段：极速档是独立 slug（-instant），
// 中/高档是 -thinking slug 配 standard/extended。因此带强度后缀的名字
// 若本身不可配强度（如 gpt-5-6-advanced 里的 gpt-5-6），要再试一次
// <base>-thinking，这样新模型族不用逐个补别名也能正确落到思考档。
func resolveFromCatalog(name string) (ResolvedModel, bool) {
	cat := CurrentModelCatalog()
	if cat == nil {
		return ResolvedModel{}, false
	}

	// 无强度后缀：直接按 slug 命中，用该模型的默认档
	if m, ok := cat.Lookup(name); ok {
		return ResolvedModel{APIModel: name, ChatModel: m.Slug, ThinkingEffort: m.DefaultEffort()}, true
	}

	base, effort := splitEffortSuffix(name)
	if effort == "" {
		return ResolvedModel{}, false
	}
	for _, candidate := range []string{base, base + "-thinking"} {
		m, ok := cat.Lookup(candidate)
		if !ok || !m.ConfigurableThinkingEffort {
			continue
		}
		eff := effort
		if !m.SupportsEffort(eff) {
			eff = m.DefaultEffort()
		}
		return ResolvedModel{APIModel: name, ChatModel: m.Slug, ThinkingEffort: eff}, true
	}
	return ResolvedModel{}, false
}

// 官网模型 UI 名称 → 后端 API 参数映射（2026-07-09 MCP 抓包实测）：
//
// 注意：以下静态表是 /backend-api/models 拉取失败时的兜底。目录可用时以目录为准，
// 见 resolveFromCatalog。官网在 2026-08 已改版为 5.5 / 5.6 两族，静态表未含 5.6。
//
// 顶层选择器（3 档，随模型族动态显示）：
//
//	UI 名称           后端 model            thinking_effort
//	极速 5.3          gpt-5-3-instant      （不携带）         ← 无论切到哪个模型族，极速档始终 gpt-5-3-instant
//	均衡              gpt-5-5-thinking     standard           ← 5.5 上下文
//	均衡（5.4族）     gpt-5-4-thinking     standard           ← 5.4 上下文
//	高级（默认）      gpt-5-5-thinking     extended           ← 5.5 上下文
//	高级（5.4族）     gpt-5-4-thinking     extended           ← 5.4 上下文
//
// 模型族子菜单（GPT-5.5 / GPT-5.4 / GPT-5.3 / o3）：
//
//	UI 名称           后端 model            thinking_effort
//	GPT-5.5           gpt-5-5-thinking     standard
//	GPT-5.4           gpt-5-3-instant      （不携带）         ← 点子菜单中的 GPT-5.4 默认进"极速"档
//	GPT-5.3           gpt-5-3-instant      （不携带）
//	o3 / Medium       o3                   （不携带）
//
// 通用规则：所有请求均携带 force_parallel_switch="auto" 和 paragen_cot_summary_display_override="allow"。

// ResolveChatModel 将请求侧的 model 名称映射到 ChatGPT 后端的真实参数。
// 生图别名（dall-e-3 / gpt-image-2*）会设置 ForcePictureV2=true 并写入后端 model。
// 非 thinking 模型（极速/gpt-5-3-instant/o3）ThinkingEffort 留空，请求体不发送该字段。
func ResolveChatModel(requestModel string) ResolvedModel {
	m := strings.TrimSpace(requestModel)
	lower := strings.ToLower(m)

	// ── 图片生成模型 ─────────────────────────────────────────────────────────────
	if strings.Contains(lower, "dall-e") {
		return ResolvedModel{
			APIModel:       m,
			ChatModel:      m,
			ForcePictureV2: true,
		}
	}
	switch lower {
	case ModelGPTImage2, ModelGPTImage2Thinking, "gpt-image-2-2026-04-21":
		return ResolvedModel{
			APIModel:       m,
			ChatModel:      ModelDALLE3,
			ForcePictureV2: true,
		}
	}
	if strings.Contains(lower, "gpt-image") {
		return ResolvedModel{
			APIModel:       m,
			ChatModel:      ModelDALLE3,
			ForcePictureV2: true,
		}
	}

	// ── 文本对话模型 ─────────────────────────────────────────────────────────────
	//
	// 优先用运行时拉取的官网目录，未命中再退回下面的静态表。

	if r, ok := resolveFromCatalog(lower); ok {
		r.APIModel = m
		return r
	}

	switch lower {
	// 官网"高级"（默认）：最强思考，触发多图并行
	case "gpt-5-5-thinking-extended", "gpt-5-5-advanced", "advanced":
		return ResolvedModel{APIModel: m, ChatModel: "gpt-5-5-thinking", ThinkingEffort: "extended"}

	// 官网"均衡" / GPT-5.5 子菜单默认：标准思考
	case "gpt-5-5-thinking", "gpt-5-5-balanced", "balanced", "gpt-5.5":
		return ResolvedModel{APIModel: m, ChatModel: "gpt-5-5-thinking", ThinkingEffort: "standard"}

	// 官网"极速"：gpt-5-5 不带 thinking_effort
	case "gpt-5-5":
		return ResolvedModel{APIModel: m, ChatModel: "gpt-5-5", ThinkingEffort: ""}

	// GPT-5.4 均衡（默认）：gpt-5-4-thinking + standard
	case "gpt-5-4", "gpt-5-4-balanced", "gpt-5.4":
		return ResolvedModel{APIModel: m, ChatModel: "gpt-5-4-thinking", ThinkingEffort: "standard"}

	// GPT-5.4 高级：gpt-5-4-thinking + extended
	case "gpt-5-4-thinking", "gpt-5-4-advanced", "gpt-5-4-extended":
		return ResolvedModel{APIModel: m, ChatModel: "gpt-5-4-thinking", ThinkingEffort: "extended"}

	// GPT-5.3：极速，不带 thinking_effort
	case "gpt-5-3", "gpt-5-3-instant", "gpt-5.3":
		return ResolvedModel{APIModel: m, ChatModel: "gpt-5-3-instant", ThinkingEffort: ""}

	// o3：不带 thinking_effort
	case "o3", "o3-medium", "o3-mini":
		return ResolvedModel{APIModel: m, ChatModel: "o3", ThinkingEffort: ""}
	}

	// 未知 model：原样透传，不携带 thinking_effort（由 chat.go resolveThinkingEffort 兜底判断）
	return ResolvedModel{
		APIModel:  m,
		ChatModel: m,
	}
}
