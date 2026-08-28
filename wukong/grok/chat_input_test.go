package grok

import (
	"encoding/json"
	"testing"
)

func TestNormalizeOpenAIInputFlattensRolesAndImages(t *testing.T) {
	messages := []chatMessage{
		{Role: "system", Content: json.RawMessage(`"be brief"`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aa"}}]`)},
	}
	got, err := normalizeOpenAIInput(messages)
	if err != nil {
		t.Fatal(err)
	}
	if got.Prompt == "" || len(got.Attachments) != 1 || !got.Attachments[0].Image {
		t.Fatalf("%#v", got)
	}
}

func TestInjectToolPrompt(t *testing.T) {
	cfg, err := parseToolConfiguration(json.RawMessage(`[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := injectToolPrompt("[user]\nhi", cfg)
	if out == "[user]\nhi" || !contains(out, "<tool_name>") {
		t.Fatalf("prompt not injected: %s", out)
	}
}
