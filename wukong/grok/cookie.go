package grok

import "strings"

const maxCloudflareCookieBytes = 8 << 10

func BuildSSOCookie(token, cloudflareCookies string) string {
	token = sanitizeSSOToken(token)
	cookies := "sso=" + token + "; sso-rw=" + token
	if sanitized := SanitizeCloudflareCookies(cloudflareCookies); sanitized != "" {
		cookies += "; " + sanitized
	}
	return cookies
}

func SanitizeCloudflareCookies(value string) string {
	allowed := make([]string, 0, 4)
	seen := make(map[string]struct{})
	for _, part := range strings.Split(value, ";") {
		name, cookieValue, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		lower := strings.ToLower(name)
		if lower != "cf_clearance" && lower != "__cf_bm" && lower != "_cfuvid" && !strings.HasPrefix(lower, "cf_chl_") {
			continue
		}
		if _, exists := seen[lower]; exists {
			continue
		}
		cookieValue = strings.TrimSpace(cookieValue)
		if cookieValue == "" || len(cookieValue) > maxCloudflareCookieBytes || strings.IndexFunc(cookieValue, func(character rune) bool {
			return character < 0x20 || character == 0x7f
		}) >= 0 {
			continue
		}
		seen[lower] = struct{}{}
		allowed = append(allowed, lower+"="+cookieValue)
	}
	return strings.Join(allowed, "; ")
}
