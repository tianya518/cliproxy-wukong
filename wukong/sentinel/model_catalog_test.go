package sentinel

import "testing"

// testCatalog 按 2026-08-24 官网 /backend-api/models 实测结构构造的目录子集。
func testCatalog() *ModelCatalog {
	c := &ModelCatalog{Models: []CatalogModel{
		{Slug: "gpt-5-5", Title: "GPT-5.5", ReasoningType: "auto"},
		{Slug: "gpt-5-5-instant", Title: "GPT-5.5 Instant", ReasoningType: "none"},
		{Slug: "gpt-5-6", Title: "GPT-5.6 Sol", ReasoningType: "auto"},
		{Slug: "gpt-5-6-instant", Title: "GPT-5.6 Sol", ReasoningType: "none"},
		{
			Slug: "gpt-5-5-thinking", Title: "GPT-5.5 Thinking", ReasoningType: "reasoning",
			ConfigurableThinkingEffort: true, ThinkingEfforts: []string{"standard", "extended"},
		},
		{
			Slug: "gpt-5-6-thinking", Title: "GPT-5.6 Sol", ReasoningType: "reasoning",
			ConfigurableThinkingEffort: true, ThinkingEfforts: []string{"standard", "extended"},
		},
		{
			Slug: "gpt-5.6-sol-wm", Title: "GPT-5.6 Sol", ReasoningType: "reasoning",
			ConfigurableThinkingEffort: true, ThinkingEfforts: []string{"standard", "extended"},
			IsWorkModeModel: true,
		},
		{Slug: "o3", Title: "o3", ReasoningType: "reasoning"},
	}}
	c.index()
	return c
}

func withCatalog(t *testing.T, c *ModelCatalog) {
	t.Helper()
	prev := CurrentModelCatalog()
	SetModelCatalog(c)
	t.Cleanup(func() { SetModelCatalog(prev) })
}

func TestResolveFromCatalog(t *testing.T) {
	withCatalog(t, testCatalog())

	cases := []struct {
		in         string
		wantModel  string
		wantEffort string
	}{
		// 新模型族：目录直接命中
		{"gpt-5-6", "gpt-5-6", ""},
		{"gpt-5-6-instant", "gpt-5-6-instant", ""},
		{"gpt-5-6-thinking", "gpt-5-6-thinking", "standard"},
		{"gpt-5-6-thinking-extended", "gpt-5-6-thinking", "extended"},
		// 带强度后缀但 base 不可配强度时，应自动落到 <base>-thinking
		{"gpt-5-6-advanced", "gpt-5-6-thinking", "extended"},
		{"gpt-5-6-balanced", "gpt-5-6-thinking", "standard"},
		{"gpt-5-6-high", "gpt-5-6-thinking", "extended"},
		// 点号写法归一化
		{"gpt-5.6", "gpt-5-6", ""},
		{"gpt-5.6-sol-wm", "gpt-5.6-sol-wm", "standard"},
		// 旧行为必须保持不变
		{"gpt-5-5-thinking", "gpt-5-5-thinking", "standard"},
		{"gpt-5-5-thinking-extended", "gpt-5-5-thinking", "extended"},
		{"gpt-5-5-advanced", "gpt-5-5-thinking", "extended"},
		{"gpt-5-5", "gpt-5-5", ""},
		{"o3", "o3", ""},
	}
	for _, tc := range cases {
		got := ResolveChatModel(tc.in)
		if got.ChatModel != tc.wantModel || got.ThinkingEffort != tc.wantEffort {
			t.Errorf("ResolveChatModel(%q) = (%q, %q), 期望 (%q, %q)",
				tc.in, got.ChatModel, got.ThinkingEffort, tc.wantModel, tc.wantEffort)
		}
		if got.APIModel != tc.in {
			t.Errorf("ResolveChatModel(%q).APIModel = %q, 应回显原名", tc.in, got.APIModel)
		}
	}
}

// TestResolveFallsBackWithoutCatalog 目录不可用时必须退回静态表，保持旧行为。
func TestResolveFallsBackWithoutCatalog(t *testing.T) {
	withCatalog(t, nil)

	cases := []struct {
		in         string
		wantModel  string
		wantEffort string
	}{
		{"gpt-5-5-thinking", "gpt-5-5-thinking", "standard"},
		{"gpt-5-5-advanced", "gpt-5-5-thinking", "extended"},
		{"gpt-5-5", "gpt-5-5", ""},
		{"gpt-5-3-instant", "gpt-5-3-instant", ""},
		{"gpt-5-4-thinking", "gpt-5-4-thinking", "extended"},
		{"o3", "o3", ""},
	}
	for _, tc := range cases {
		got := ResolveChatModel(tc.in)
		if got.ChatModel != tc.wantModel || got.ThinkingEffort != tc.wantEffort {
			t.Errorf("无目录时 ResolveChatModel(%q) = (%q, %q), 期望 (%q, %q)",
				tc.in, got.ChatModel, got.ThinkingEffort, tc.wantModel, tc.wantEffort)
		}
	}
}

// TestCatalogDoesNotHijackImageModels 生图别名不能被目录路径截走。
func TestCatalogDoesNotHijackImageModels(t *testing.T) {
	withCatalog(t, testCatalog())
	for _, name := range []string{"dall-e-3", "gpt-image-2", "gpt-image-2-thinking"} {
		got := ResolveChatModel(name)
		if !got.ForcePictureV2 {
			t.Errorf("ResolveChatModel(%q).ForcePictureV2 = false, 生图别名应强制 picture_v2", name)
		}
	}
}

// TestUnknownModelPassthrough 目录与静态表都没有的名字应原样透传。
func TestUnknownModelPassthrough(t *testing.T) {
	withCatalog(t, testCatalog())
	got := ResolveChatModel("some-future-model")
	if got.ChatModel != "some-future-model" || got.ThinkingEffort != "" {
		t.Errorf("未知 model 应透传，实得 (%q, %q)", got.ChatModel, got.ThinkingEffort)
	}
}

func TestHideFromChatCatalog(t *testing.T) {
	if !(CatalogModel{Slug: "research"}).HideFromChatCatalog() {
		t.Error("research 应隐藏")
	}
	if !(CatalogModel{Slug: "gpt-5.6-sol-wm", IsWorkModeModel: true}).HideFromChatCatalog() {
		t.Error("工作模式模型应隐藏")
	}
	if (CatalogModel{Slug: "gpt-5-6-thinking"}).HideFromChatCatalog() {
		t.Error("普通聊天模型不应隐藏")
	}
}

func TestCatalogModelEffortHelpers(t *testing.T) {
	cfgable := CatalogModel{ConfigurableThinkingEffort: true, ThinkingEfforts: []string{"standard", "extended"}}
	if got := cfgable.DefaultEffort(); got != "standard" {
		t.Errorf("DefaultEffort() = %q, 期望 standard（目录首项即默认档）", got)
	}
	if !cfgable.SupportsEffort("extended") || cfgable.SupportsEffort("ultra") {
		t.Error("SupportsEffort 判定错误")
	}

	plain := CatalogModel{ReasoningType: "none"}
	if got := plain.DefaultEffort(); got != "" {
		t.Errorf("不可配强度的模型 DefaultEffort() = %q, 应为空（请求体不带该字段）", got)
	}
}

func TestSplitEffortSuffix(t *testing.T) {
	cases := map[string][2]string{
		"gpt-5-6-thinking-extended": {"gpt-5-6-thinking", "extended"},
		"gpt-5-6-advanced":          {"gpt-5-6", "extended"},
		"gpt-5-6-balanced":          {"gpt-5-6", "standard"},
		"gpt-5-6-thinking":          {"gpt-5-6-thinking", ""},
		"o3":                        {"o3", ""},
	}
	for in, want := range cases {
		base, eff := splitEffortSuffix(in)
		if base != want[0] || eff != want[1] {
			t.Errorf("splitEffortSuffix(%q) = (%q, %q), 期望 (%q, %q)", in, base, eff, want[0], want[1])
		}
	}
}
