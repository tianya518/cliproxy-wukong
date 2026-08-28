package grok

import "testing"

func TestParseCredentialsJSON(t *testing.T) {
	raw := []byte(`{"provider":"grok_web","accounts":[{"name":"a","sso_token":"tok-1","user_id":"497f19f8-49d4-458a-bee4-43ec3dcaf8ca","tier":"super"}]}`)
	got, err := ParseCredentials(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].AccessToken() != "tok-1" || got[0].WebTier() != TierSuper {
		t.Fatalf("%#v", got)
	}
}

func TestParseCredentialsPlainAndCookiePrefix(t *testing.T) {
	got, err := ParseCredentials([]byte("sso=abc; Path=/\n\ndef\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].AccessToken() != "abc" || got[1].AccessToken() != "def" {
		t.Fatalf("%#v", got)
	}
}

func TestParseCredentialsRejectsWrongProvider(t *testing.T) {
	_, err := ParseCredentials([]byte(`{"provider":"grok_build","accounts":[{"sso_token":"x"}]}`))
	if err == nil {
		t.Fatal("expected provider mismatch")
	}
}

func TestParseUploadShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"plain", "sso=abc\n", "abc"},
		{"sso_field", `{"sso":"tok-2","name":"plus","tier":"super"}`, "tok-2"},
		{"accounts_string", `{"accounts":"sso=tok-3","cloudflare_cookies":"cf_clearance=z"}`, "tok-3"},
		{"document", `{"provider":"grok_web","accounts":[{"sso_token":"tok-4"}]}`, "tok-4"},
	}
	for _, tc := range cases {
		got, err := ParseUpload([]byte(tc.raw))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != 1 || got[0].AccessToken() != tc.want {
			t.Fatalf("%s: %#v", tc.name, got)
		}
	}
}
