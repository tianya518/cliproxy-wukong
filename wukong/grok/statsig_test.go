package grok

import "testing"

func TestExtractStatsigMetaContentAcceptsUnicodeDash(t *testing.T) {
	for _, name := range []string{"grok-site\u2015verification", "grok-site-verification"} {
		body := []byte(`<html><head><meta name="` + name + `" content="meta-value"/></head></html>`)
		value, err := extractStatsigMetaContent(body)
		if err != nil || value != "meta-value" {
			t.Fatalf("name=%q value=%q err=%v", name, value, err)
		}
	}
}
