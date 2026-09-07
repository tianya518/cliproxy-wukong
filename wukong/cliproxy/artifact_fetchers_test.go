package cliproxy

import (
	"context"
	"errors"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"

	"github.com/router-for-me/CLIProxyAPI/v7/wukong/grok"
	sentinelserver "github.com/router-for-me/CLIProxyAPI/v7/wukong/server"
)

func registerTestAuth(t *testing.T, mgr *coreauth.Manager, id, provider string, disabled bool) {
	t.Helper()
	auth := &coreauth.Auth{ID: id, Provider: provider, Status: coreauth.StatusActive, Disabled: disabled,
		Metadata: map[string]any{"type": provider, "sso_token": "sso-" + id, "access_token": "at-" + id}}
	if disabled {
		auth.Status = coreauth.StatusDisabled
	}
	if _, err := mgr.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatal(err)
	}
}

func TestCandidateAuthsHonorsAuthIDAndProvider(t *testing.T) {
	mgr := coreauth.NewManager(nil, nil, nil)
	registerTestAuth(t, mgr, "grok-web-a", GrokProviderKey, false)
	registerTestAuth(t, mgr, "grok-web-b", GrokProviderKey, false)
	registerTestAuth(t, mgr, "grok-web-off", GrokProviderKey, true)
	registerTestAuth(t, mgr, "chatgpt-web-1", ProviderKey, false)

	// 指定凭证：只用它，不跨账号
	got := candidateAuths(mgr, GrokProviderKey, "grok-web-a")
	if len(got) != 1 || got[0].ID != "grok-web-a" {
		t.Fatalf("by id = %v", ids(got))
	}
	// 指定的凭证已不存在 / 已禁用：不退回别的账号
	if got := candidateAuths(mgr, GrokProviderKey, "grok-web-gone"); len(got) != 0 {
		t.Fatalf("missing id must yield none, got %v", ids(got))
	}
	if got := candidateAuths(mgr, GrokProviderKey, "grok-web-off"); len(got) != 0 {
		t.Fatalf("disabled id must yield none, got %v", ids(got))
	}
	// 没记凭证 ID（旧记录）：同 provider 的启用凭证都算候选，其它 provider 不算
	got = candidateAuths(mgr, GrokProviderKey, "")
	if len(got) != 2 {
		t.Fatalf("by provider = %v", ids(got))
	}
	for _, a := range got {
		if a.Provider != GrokProviderKey || a.Disabled {
			t.Fatalf("unexpected candidate %+v", a)
		}
	}
	if got := candidateAuths(nil, GrokProviderKey, ""); got != nil {
		t.Fatal("nil manager must yield nil")
	}
}

func TestArtifactFetchersRejectIncompleteOrOrphanRecords(t *testing.T) {
	mgr := coreauth.NewManager(nil, nil, nil)
	ctx := context.Background()

	gf := NewGrokArtifactFetcher(mgr, grok.Config{})
	if _, _, err := gf.Fetch(ctx, sentinelserver.ArtifactRecord{ID: "x", Provider: GrokProviderKey}); err == nil {
		t.Fatal("missing url must fail")
	}
	_, _, err := gf.Fetch(ctx, sentinelserver.ArtifactRecord{ID: "x", Provider: GrokProviderKey, AuthID: "gone",
		Locator: sentinelserver.ArtifactLocator{URL: "https://assets.grok.com/a.jpg"}})
	if !errors.Is(err, ErrArtifactCredentialMissing) {
		t.Fatalf("orphan record should report credential missing, got %v", err)
	}

	cf := NewChatGPTArtifactFetcher(mgr, &sentinelserver.ServerConfig{})
	if _, _, err := cf.Fetch(ctx, sentinelserver.ArtifactRecord{ID: "y", Provider: ProviderKey,
		Locator: sentinelserver.ArtifactLocator{FileID: "file_1"}}); err == nil {
		t.Fatal("missing conv_id must fail")
	}
	_, _, err = cf.Fetch(ctx, sentinelserver.ArtifactRecord{ID: "y", Provider: ProviderKey, AuthID: "gone",
		Locator: sentinelserver.ArtifactLocator{ConvID: "c", FileID: "file_1"}})
	if !errors.Is(err, ErrArtifactCredentialMissing) {
		t.Fatalf("orphan record should report credential missing, got %v", err)
	}
}

func ids(auths []*coreauth.Auth) []string {
	out := make([]string, 0, len(auths))
	for _, a := range auths {
		out = append(out, a.ID)
	}
	return out
}
