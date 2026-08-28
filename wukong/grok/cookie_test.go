package grok

import "testing"

func TestBuildSSOCookie(t *testing.T) {
	got := BuildSSOCookie("test-sso", "cf_clearance=test-cf; __cf_bm=bm; other=x")
	for _, want := range []string{"sso=test-sso", "sso-rw=test-sso", "cf_clearance=test-cf", "__cf_bm=bm"} {
		if !contains(got, want) {
			t.Fatalf("cookie %q missing %q", got, want)
		}
	}
	if contains(got, "other=x") {
		t.Fatalf("unexpected third-party cookie: %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || (len(s) > 0 && (stringIndex(s, sub) >= 0)))
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
