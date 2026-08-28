package grok

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSignStatsigFallsBackToRemoteWhenCurvesMissing(t *testing.T) {
	want := base64.RawStdEncoding.EncodeToString(make([]byte, 70))
	var gotMethod, gotPath, gotMeta string
	signer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Method      string            `json:"method"`
			Path        string            `json:"path"`
			Environment map[string]string `json:"environment"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode signer payload: %v", err)
			http.Error(writer, "bad json", http.StatusBadRequest)
			return
		}
		gotMethod, gotPath, gotMeta = payload.Method, payload.Path, payload.Environment["metaContent"]
		_ = json.NewEncoder(writer).Encode(map[string]string{"x-statsig-id": want})
	}))
	defer signer.Close()

	page := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`<html><head><meta name="grok-site-verification" content="meta-from-page"/></head><body>no curves</body></html>`))
	}))
	defer page.Close()

	client := NewClient(Config{
		BaseURL:          page.URL,
		StatsigMode:      StatsigModeURL,
		StatsigSignerURL: signer.URL,
	}, Credential{SSOToken: "test-sso"})
	got, err := client.signStatsig(context.Background(), http.MethodPost, page.URL+"/rest/app-chat/conversations/new")
	if err != nil || got != want {
		t.Fatalf("signed=%q err=%v", got, err)
	}
	if gotMethod != "POST" || gotPath != "/rest/app-chat/conversations/new" || gotMeta != "meta-from-page" {
		t.Fatalf("method=%q path=%q meta=%q", gotMethod, gotPath, gotMeta)
	}
}

func TestApplySignedStatsigUsesManualValue(t *testing.T) {
	value := base64.RawStdEncoding.EncodeToString(make([]byte, 70))
	client := NewClient(Config{StatsigMode: StatsigModeManual, StatsigManualValue: value}, Credential{SSOToken: "test-sso"})
	headers := http.Header{}
	client.applySignedStatsig(context.Background(), headers, http.MethodPost, "https://grok.com/rest/test")
	if headers.Get("x-statsig-id") != value {
		t.Fatalf("x-statsig-id = %q", headers.Get("x-statsig-id"))
	}
}

func TestApplySignedStatsigNeverLeavesInvalidManual(t *testing.T) {
	client := NewClient(Config{StatsigMode: StatsigModeManual, StatsigManualValue: "invalid"}, Credential{SSOToken: "test-sso"})
	headers := http.Header{}
	headers.Set("x-statsig-id", "random-fallback")
	err := client.applySignedStatsig(context.Background(), headers, http.MethodPost, "https://grok.com/rest/test")
	if err == nil {
		t.Fatal("expected invalid manual statsig to fail")
	}
	if value := headers.Get("x-statsig-id"); value != "" {
		t.Fatalf("x-statsig-id = %q", value)
	}
}

func TestSignStatsigDefaultLocalDoesNotFallbackRemote(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`<html><head><meta name="grok-site-verification" content="meta-from-page"/></head><body>no curves</body></html>`))
	}))
	defer page.Close()

	client := NewClient(Config{BaseURL: page.URL}, Credential{SSOToken: "test-sso"})
	_, err := client.signStatsig(context.Background(), http.MethodPost, page.URL+"/rest/app-chat/conversations/new")
	if err == nil {
		t.Fatal("expected local statsig to fail without remote fallback")
	}
	if !strings.Contains(err.Error(), "Botox") && !strings.Contains(err.Error(), "曲线") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateStatsigSignerURL(t *testing.T) {
	if err := validateStatsigSignerURL(DefaultStatsigSignerURL); err != nil {
		t.Fatal(err)
	}
	if err := validateStatsigSignerURL("http://127.0.0.1:8788/sign"); err != nil {
		t.Fatal(err)
	}
	if err := validateStatsigSignerURL("https://evil.example/sign?x=1"); err == nil {
		t.Fatal("query must be rejected")
	}
}

func TestNormalizedConfigDefaultsStatsigLocalMode(t *testing.T) {
	cfg := Config{}.normalized()
	if cfg.StatsigMode != StatsigModeLocal {
		t.Fatalf("mode=%q", cfg.StatsigMode)
	}
	if cfg.StatsigSignerURL != "" {
		t.Fatalf("default signer url = %q", cfg.StatsigSignerURL)
	}
}
