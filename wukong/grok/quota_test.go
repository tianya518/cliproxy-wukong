package grok

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newQuotaTestClient(t *testing.T, handler http.HandlerFunc) (*Client, func()) {
	t.Helper()
	server := httptest.NewServer(handler)
	cfg := Config{
		BaseURL:            server.URL,
		StatsigMode:        StatsigModeManual,
		StatsigManualValue: base64.RawStdEncoding.EncodeToString(make([]byte, 70)),
	}
	return NewClient(cfg, Credential{SSOToken: "test-sso"}), server.Close
}

// 官网把 remainingQueries/totalQueries 换成 *Tokens 时不应整批失败。
func TestSyncQuotaModeAcceptsTokenFieldNames(t *testing.T) {
	for name, body := range map[string]string{
		"queries": `{"windowSizeSeconds":7200,"remainingQueries":11,"totalQueries":20}`,
		"tokens":  `{"windowSizeSeconds":7200,"remainingTokens":11,"totalTokens":20}`,
		"mixed":   `{"windowSizeSeconds":7200,"remainingTokens":11,"totalQueries":20}`,
	} {
		t.Run(name, func(t *testing.T) {
			client, closeServer := newQuotaTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/rest/rate-limits" {
					http.NotFound(writer, request)
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(body))
			})
			defer closeServer()

			window, err := client.SyncQuotaMode(context.Background(), "auto")
			if err != nil {
				t.Fatal(err)
			}
			if window.Remaining != 11 || window.Total != 20 || window.WindowSeconds != 7200 {
				t.Fatalf("window = %#v", window)
			}
			// 上游没给重置时间就不要造一个出来。
			if window.ResetAt != nil {
				t.Fatalf("reset_at should stay empty without waitTimeSeconds: %v", window.ResetAt)
			}
		})
	}
}

// 被限流时上游带 waitTimeSeconds，这才是唯一可信的重置时间。
func TestSyncQuotaModeUsesWaitTimeSecondsForReset(t *testing.T) {
	client, closeServer := newQuotaTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"windowSizeSeconds":7200,"remainingQueries":0,"totalQueries":20,"waitTimeSeconds":900}`))
	})
	defer closeServer()

	before := time.Now()
	window, err := client.SyncQuotaMode(context.Background(), "auto")
	if err != nil {
		t.Fatal(err)
	}
	if window.ResetAt == nil {
		t.Fatal("reset_at missing despite waitTimeSeconds")
	}
	delta := window.ResetAt.Sub(before)
	if delta < 14*time.Minute || delta > 16*time.Minute {
		t.Fatalf("reset_at offset = %v, want ~15m", delta)
	}
}

// 只剩 remaining 也算可用信息，只有两个都缺才是解析失败。
func TestSyncQuotaModeRequiresRemainingOrTotal(t *testing.T) {
	client, closeServer := newQuotaTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"remainingQueries":7}`))
	})
	defer closeServer()

	window, err := client.SyncQuotaMode(context.Background(), "auto")
	if err != nil {
		t.Fatal(err)
	}
	if window.Remaining != 7 || window.Total != 0 || window.WindowSeconds != 7200 {
		t.Fatalf("window = %#v", window)
	}

	empty, closeEmpty := newQuotaTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"windowSizeSeconds":7200}`))
	})
	defer closeEmpty()

	if _, err = empty.SyncQuotaMode(context.Background(), "auto"); err == nil {
		t.Fatal("expected an error when both remaining and total are absent")
	}
}

func TestSyncImagineQuotaSkipsProductsWithoutAvailable(t *testing.T) {
	client, closeServer := newQuotaTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/rest/media/imagine/quota_info" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"image":{"available":true,"windowSizeSeconds":64800},
			"imagePro":{"available":true,"remainingQueries":3,"windowSizeSeconds":86400},
			"imageEdit":{"remainingQueries":9},
			"video":{"available":false}
		}`))
	})
	defer closeServer()

	windows, err := client.SyncImagineQuota(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 3 {
		t.Fatalf("windows = %#v", windows)
	}
	if windows[0].Mode != "image" || windows[0].Available == nil || !*windows[0].Available || windows[0].WindowSeconds != 64800 {
		t.Fatalf("image = %#v", windows[0])
	}
	if windows[1].Mode != "image_pro" || windows[1].Remaining != 3 || windows[1].Available == nil || !*windows[1].Available {
		t.Fatalf("imagePro = %#v", windows[1])
	}
	if windows[2].Mode != "video" || windows[2].Available == nil || *windows[2].Available {
		t.Fatalf("video = %#v", windows[2])
	}
}

func TestQuotaForReportsErrorInsteadOfPanicking(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "nope", http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := Config{
		BaseURL:            server.URL,
		StatsigMode:        StatsigModeManual,
		StatsigManualValue: base64.RawStdEncoding.EncodeToString(make([]byte, 70)),
	}
	result := QuotaFor(context.Background(), cfg, Credential{Name: "acc", SSOToken: "test-sso"})
	if result.Error == "" {
		t.Fatal("expected the upstream failure to surface as an error string")
	}
	if result.Name != "acc" || result.ID == "" {
		t.Fatalf("result = %#v", result)
	}
	if result.SyncedAt != nil {
		t.Fatalf("synced_at should stay empty on failure: %#v", result.SyncedAt)
	}
}
