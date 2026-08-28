package grok

import (
	"net/http"
	"testing"
)

func TestIsClearanceRefreshableMediaError(t *testing.T) {
	tests := []struct {
		name string
		body string
		code int
		want bool
	}{
		{name: "empty challenge response", code: http.StatusForbidden, want: true},
		{name: "cloudflare html", code: http.StatusForbidden, body: "<!doctype html><title>Just a moment...</title>", want: true},
		{name: "structured moderation response", code: http.StatusForbidden, body: `{"error":{"code":"content-moderated","message":"rejected"}}`, want: false},
		{name: "statsig page out of date", code: http.StatusForbidden, body: `{"error":{"code":7,"message":"This page is out of date. Reload to continue."}}`, want: false},
		{name: "server failure", code: http.StatusBadGateway, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := newWebMediaUpstreamError(test.code, []byte(test.body))
			if got := isClearanceRefreshableMediaError(err); got != test.want {
				t.Fatalf("refreshable=%v, want %v (kind=%q challenge=%v)", got, test.want, err.bodyKind, err.cloudflareChallenge)
			}
		})
	}
}

func TestIsStatsigRefreshableMediaError(t *testing.T) {
	body := []byte(`{"error":{"code":7,"message":"This page is out of date. Reload to continue."}}`)
	if !isStatsigRefreshableMediaError(newWebMediaUpstreamError(http.StatusForbidden, body), body) {
		t.Fatal("expected statsig retry")
	}
	blocked := []byte(`{"error":{"code":7,"message":"user is blocked"}}`)
	if isStatsigRefreshableMediaError(newWebMediaUpstreamError(http.StatusForbidden, blocked), blocked) {
		t.Fatal("blocked account must stay terminal")
	}
}
