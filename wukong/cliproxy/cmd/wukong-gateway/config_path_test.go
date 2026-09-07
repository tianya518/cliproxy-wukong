package main

import "testing"

func TestParseGatewayArgs(t *testing.T) {
	getenv := func(string) string { return "" }

	t.Run("baota command", func(t *testing.T) {
		path, rest := parseGatewayArgs([]string{"--config", "config.yaml"}, getenv)
		if path != "config.yaml" {
			t.Fatalf("path=%q", path)
		}
		if len(rest) != 0 {
			t.Fatalf("rest=%v", rest)
		}
	})

	t.Run("bare --config keeps default", func(t *testing.T) {
		path, rest := parseGatewayArgs([]string{"--config"}, getenv)
		if path != defaultConfigPath {
			t.Fatalf("path=%q", path)
		}
		if len(rest) != 0 {
			t.Fatalf("rest=%v", rest)
		}
	})

	t.Run("equals form", func(t *testing.T) {
		path, _ := parseGatewayArgs([]string{"--config=/www/wwwroot/CLIProxyAPI/config.yaml"}, getenv)
		if path != "/www/wwwroot/CLIProxyAPI/config.yaml" {
			t.Fatalf("path=%q", path)
		}
	})

	t.Run("flag before login", func(t *testing.T) {
		path, rest := parseGatewayArgs([]string{"--config", "prod.yaml", "login", "codex"}, getenv)
		if path != "prod.yaml" {
			t.Fatalf("path=%q", path)
		}
		if len(rest) != 2 || rest[0] != "login" || rest[1] != "codex" {
			t.Fatalf("rest=%v", rest)
		}
	})

	t.Run("CLIPROXY_CONFIG fallback", func(t *testing.T) {
		path, _ := parseGatewayArgs(nil, func(k string) string {
			if k == "CLIPROXY_CONFIG" {
				return "/etc/cliproxy.yaml"
			}
			return ""
		})
		if path != "/etc/cliproxy.yaml" {
			t.Fatalf("path=%q", path)
		}
	})

	t.Run("flag overrides env", func(t *testing.T) {
		path, _ := parseGatewayArgs([]string{"-config", "flag.yaml"}, func(string) string {
			return "/etc/cliproxy.yaml"
		})
		if path != "flag.yaml" {
			t.Fatalf("path=%q", path)
		}
	})
}
