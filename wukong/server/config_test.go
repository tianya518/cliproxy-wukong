package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveChatGPTFilePrefersNewName(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	t.Setenv("CHATGPT_FILE", "")
	t.Setenv("TOKENS_FILE", "")
	if got := resolveChatGPTFile(); got != "chatgpt.json" {
		t.Fatalf("empty dir default = %q", got)
	}

	if err := os.WriteFile("tokens.json", []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveChatGPTFile(); got != "tokens.json" {
		t.Fatalf("legacy file = %q", got)
	}

	if err := os.WriteFile("chatgpt.json", []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveChatGPTFile(); got != "chatgpt.json" {
		t.Fatalf("new file should win = %q", got)
	}

	t.Setenv("TOKENS_FILE", filepath.Join(dir, "custom-tokens.json"))
	if got := resolveChatGPTFile(); got != filepath.Join(dir, "custom-tokens.json") {
		t.Fatalf("TOKENS_FILE = %q", got)
	}

	t.Setenv("CHATGPT_FILE", filepath.Join(dir, "explicit.json"))
	if got := resolveChatGPTFile(); got != filepath.Join(dir, "explicit.json") {
		t.Fatalf("CHATGPT_FILE = %q", got)
	}
}
