package grok

import (
	"net/url"
	"strings"
	"unicode"
)

const (
	maxURLBytes   = 8 << 10
	maxResults    = 50
	maxTitleRunes = 512
)

func normalizeURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURLBytes || strings.IndexFunc(raw, unsafeTextRune) >= 0 {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.Hostname() == "" || parsed.User != nil {
		return "", false
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", false
	}
	parsed.Host = strings.ToLower(parsed.Host)
	normalized := parsed.String()
	if len(normalized) > maxURLBytes {
		return "", false
	}
	return normalized, true
}

func normalizeTitle(raw, fallback string) string {
	value := sanitizeTitle(raw)
	if value == "" {
		value = sanitizeTitle(fallback)
	}
	runes := []rune(value)
	if len(runes) > maxTitleRunes {
		value = string(runes[:maxTitleRunes])
	}
	return value
}

func unsafeTextRune(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf)
}

func sanitizeTitle(value string) string {
	value = strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', '\t':
			return ' '
		}
		if unsafeTextRune(r) {
			return -1
		}
		return r
	}, value)
	return strings.Join(strings.Fields(value), " ")
}
