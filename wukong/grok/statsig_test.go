package grok

import "testing"

func TestStatsigRequestPathUsesEscapedPathOnly(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://grok.com/rest/app-chat/conversations/new?x=1", "/rest/app-chat/conversations/new"},
		{"/rest/modes?foo=bar", "/rest/modes"},
		{"https://grok.com", "/"},
		{"", "/"},
	}
	for _, tc := range cases {
		if got := statsigRequestPath(tc.in); got != tc.want {
			t.Fatalf("statsigRequestPath(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestExtractStatsigMetaContentAcceptsUnicodeDash(t *testing.T) {
	for _, name := range []string{"grok-site\u2015verification", "grok-site-verification"} {
		body := []byte(`<html><head><meta name="` + name + `" content="meta-value"/></head></html>`)
		value, err := extractStatsigMetaContent(body)
		if err != nil || value != "meta-value" {
			t.Fatalf("name=%q value=%q err=%v", name, value, err)
		}
	}
}
