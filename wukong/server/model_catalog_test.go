package server

import (
	"strings"
	"testing"

	sentinel "github.com/router-for-me/CLIProxyAPI/v7/wukong/sentinel"
)

func TestBuildModelListHidesWorkAndResearch(t *testing.T) {
	cat := &sentinel.ModelCatalog{Models: []sentinel.CatalogModel{
		{Slug: "gpt-5-6-instant"},
		{
			Slug: "gpt-5-6-thinking", ConfigurableThinkingEffort: true,
			ThinkingEfforts: []string{"standard", "extended"},
		},
		{Slug: "research"},
		{Slug: "gpt-5.6-sol-wm", IsWorkModeModel: true},
	}}
	got := buildModelList(cat)
	joined := make([]string, 0, len(got))
	for _, m := range got {
		joined = append(joined, m.ID)
	}
	s := strings.Join(joined, ",")
	for _, want := range []string{"gpt-5-6-instant", "gpt-5-6-thinking", "gpt-5-6-thinking-extended", sentinel.ModelDALLE3} {
		if !strings.Contains(s, want) {
			t.Errorf("缺少 %q：%v", want, joined)
		}
	}
	for _, id := range joined {
		if id == "research" || strings.Contains(id, "-wm") {
			t.Errorf("%q 不应出现在 /v1/models", id)
		}
	}
}
