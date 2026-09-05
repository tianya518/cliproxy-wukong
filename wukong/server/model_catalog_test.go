package server

import (
	"errors"
	"strings"
	"testing"

	sentinel "github.com/router-for-me/CLIProxyAPI/v7/wukong/sentinel"
)

func TestCatalogAuthFailure(t *testing.T) {
	if !isCatalogAuthFailure(errors.New(`fetch models: http 401: {"detail":"Could not parse your authentication token"}`)) {
		t.Fatal("401 should be treated as a dead token")
	}
	if isCatalogAuthFailure(errors.New("fetch models: http 500: upstream")) {
		t.Fatal("5xx should not mark the auth as invalid")
	}
	msg := catalogAuthFailureMessage(errors.New("fetch models: http 401: token"))
	if !strings.Contains(msg, "token 无效") || !strings.Contains(msg, "http 401") {
		t.Fatalf("message = %q", msg)
	}
}

func TestCatalogStatusFallback(t *testing.T) {
	prev := sentinel.CurrentCatalogStatus()
	t.Cleanup(func() {
		sentinel.SetCatalogStatus(prev.Source, prev.Error, prev.SyncedAt)
	})
	setCatalogFallback(errors.New("fetch models: http 401: no"))
	got := CatalogStatus()
	if got.Source != sentinel.CatalogSourceFallback || !strings.Contains(got.Error, "http 401") {
		t.Fatalf("%+v", got)
	}
	setCatalogLive()
	got = CatalogStatus()
	if got.Source != sentinel.CatalogSourceLive || got.Error != "" || got.SyncedAt.IsZero() {
		t.Fatalf("%+v", got)
	}
}

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
