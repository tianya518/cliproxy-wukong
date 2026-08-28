package grok

import (
	"net/http"
	"strings"
)

const (
	DefaultBaseURL = "https://grok.com"
	// grok2api egress.DefaultUserAgent，网页通道按这套 UA 对过 Cloudflare / Statsig。
	DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
)

func applyAppHeaders(value http.Header, origin, referer string) {
	value.Set("Origin", origin)
	value.Set("Referer", referer)
	value.Set("Cache-Control", "no-cache")
	value.Set("Pragma", "no-cache")
	value.Set("Priority", "u=1, i")
	value.Set("Sec-Fetch-Dest", "empty")
	value.Set("Sec-Fetch-Mode", "cors")
	value.Set("Sec-Fetch-Site", "same-origin")
}

func (c *Client) restHeaders(contentType, referer string) http.Header {
	if contentType == "" {
		contentType = "application/json"
	}
	value := http.Header{}
	value.Set("Content-Type", contentType)
	value.Set("Accept", "*/*")
	value.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	value.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	value.Set("User-Agent", c.userAgent())
	value.Set("Cookie", c.cookieHeader(""))
	value.Set("x-xai-request-id", newRequestUUID())
	origin := strings.TrimRight(c.baseURL(), "/")
	if referer == "" {
		referer = origin + "/"
	}
	applyAppHeaders(value, origin, referer)
	return value
}

func (c *Client) sessionHeaders() http.Header {
	origin := strings.TrimRight(c.baseURL(), "/")
	value := http.Header{}
	value.Set("Accept", "*/*")
	value.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	value.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	value.Set("Cache-Control", "no-cache")
	value.Set("Cookie", c.cookieHeader(""))
	value.Set("Pragma", "no-cache")
	value.Set("Priority", "u=1, i")
	value.Set("Referer", origin+"/")
	value.Set("Sec-Fetch-Dest", "empty")
	value.Set("Sec-Fetch-Mode", "cors")
	value.Set("Sec-Fetch-Site", "same-origin")
	value.Set("User-Agent", c.userAgent())
	return value
}

func (c *Client) cookieHeader(userID string) string {
	cookie := BuildSSOCookie(c.cred.AccessToken(), c.cloudflareCookies())
	if userID != "" {
		cookie += "; x-userid=" + userID
	}
	return cookie
}

func (c *Client) userAgent() string {
	if ua := strings.TrimSpace(c.cred.UserAgent); ua != "" {
		return ua
	}
	if ua := strings.TrimSpace(c.cfg.UserAgent); ua != "" {
		return ua
	}
	return DefaultUserAgent
}

func (c *Client) baseURL() string {
	if u := strings.TrimSpace(c.cfg.BaseURL); u != "" {
		return strings.TrimRight(u, "/")
	}
	return DefaultBaseURL
}
