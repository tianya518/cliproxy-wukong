// Command refresh-live hits OpenAI/ChatGPT to renew one chatgpt.json / tokens.json
// account. Prints only id / path / expiry — never the tokens themselves.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"

	glue "github.com/router-for-me/CLIProxyAPI/v7/wukong/cliproxy"
	sentinelserver "github.com/router-for-me/CLIProxyAPI/v7/wukong/server"
)

func main() {
	path := ""
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "usage: refresh-live <chatgpt.json|tokens.json>")
		os.Exit(2)
	}
	creds, err := sentinelserver.LoadChatGPTCredentials(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load %s: %v\n", path, err)
		os.Exit(1)
	}
	if len(creds) == 0 {
		fmt.Fprintf(os.Stderr, "%s: no credentials\n", path)
		os.Exit(1)
	}

	exec := glue.NewExecutor(nil, "")
	failed := 0
	for i, cred := range creds {
		id := cred.ID
		if id == "" {
			id = fmt.Sprintf("#%d", i+1)
		}
		fmt.Printf("account %s  rt=%t st=%t at=%t  exp_before=%s\n",
			id, cred.RefreshToken != "", cred.SessionToken != "", cred.AccessToken != "",
			formatExp(cred.ExpiresAt))

		auth := &coreauth.Auth{
			ID:       "chatgpt-web-" + id,
			Provider: glue.ProviderKey,
			Metadata: map[string]any{},
		}
		if cred.AccessToken != "" {
			auth.Metadata["access_token"] = cred.AccessToken
		}
		if cred.RefreshToken != "" {
			auth.Metadata["refresh_token"] = cred.RefreshToken
		}
		if cred.SessionToken != "" {
			auth.Metadata["session_token"] = cred.SessionToken
		}

		started := time.Now()
		got, refreshErr := exec.Refresh(context.Background(), auth)
		elapsed := time.Since(started).Round(time.Millisecond)
		if refreshErr != nil {
			failed++
			fmt.Printf("  FAIL  %s  (%s)\n", sanitizeErr(refreshErr), elapsed)
			continue
		}
		newAT, _ := got.Metadata["access_token"].(string)
		newRT, _ := got.Metadata["refresh_token"].(string)
		expStr, _ := got.Metadata["expired"].(string)
		changed := cred.AccessToken != "" && newAT != "" && newAT != cred.AccessToken
		fmt.Printf("  OK    at_len=%d at_changed=%t rt_rotated=%t exp_after=%s  (%s)\n",
			len(newAT), changed, newRT != "" && newRT != cred.RefreshToken, expStr, elapsed)
	}
	if failed > 0 {
		os.Exit(1)
	}
}

func formatExp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format(time.RFC3339)
}

func sanitizeErr(err error) string {
	s := err.Error()
	for _, needle := range []string{"eyJ", "rt_", "sk-"} {
		if i := strings.Index(s, needle); i >= 0 {
			return strings.TrimSpace(s[:i]) + "[redacted]"
		}
	}
	if len(s) > 240 {
		return s[:240] + "…"
	}
	return s
}
