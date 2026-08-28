package cliproxy

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"

	"github.com/router-for-me/CLIProxyAPI/v7/wukong/grok"
)

func TestReplaceGrokAuthsKeepsChatGPT(t *testing.T) {
	ctx := context.Background()
	mgr := coreauth.NewManager(nil, nil, nil)
	_, err := mgr.Register(ctx, &coreauth.Auth{
		ID:       ProviderKey + "-keep",
		Provider: ProviderKey,
		Status:   coreauth.StatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = RegisterGrokAuths(ctx, mgr, []grok.Credential{{SSOToken: "old-sso", Name: "old"}}); err != nil {
		t.Fatal(err)
	}
	if err = ReplaceGrokAuths(ctx, mgr, []grok.Credential{{SSOToken: "new-sso", Name: "new"}}); err != nil {
		t.Fatal(err)
	}

	var chatgpt, grokN int
	for _, auth := range mgr.List() {
		switch auth.Provider {
		case ProviderKey:
			chatgpt++
			if auth.ID != ProviderKey+"-keep" {
				t.Fatalf("chatgpt auth mutated: %s", auth.ID)
			}
		case GrokProviderKey:
			grokN++
			if auth.Attributes["sso_token"] != "new-sso" {
				t.Fatalf("grok auth %#v", auth)
			}
		}
	}
	if chatgpt != 1 || grokN != 1 {
		t.Fatalf("chatgpt=%d grok=%d", chatgpt, grokN)
	}
}
