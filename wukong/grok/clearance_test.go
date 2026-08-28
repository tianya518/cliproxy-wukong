package grok

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testEditPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

func TestImageEditReplaysWholeFlowWithRefreshedClearance(t *testing.T) {
	for _, challengeStage := range []string{"upload", "generation"} {
		t.Run(challengeStage, func(t *testing.T) {
			var solverCalls atomic.Int32
			var uploadCalls atomic.Int32
			var generationCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/v1":
					writeTestClearanceSolution(t, writer, solverCalls.Add(1))
				case "/http/upload-file-v2/direct":
					call := uploadCalls.Add(1)
					assertTestClearanceCookie(t, request, solverCalls.Load())
					if challengeStage == "upload" && call == 1 {
						writeTestChallenge(writer)
						return
					}
					writer.Header().Set("Content-Type", "application/json")
					_, _ = writer.Write([]byte(`{"uploadId":"upload-1","fileMetadata":{"fileMetadataId":"file-1","fileUri":"users/test/reference/content"}}`))
				case "/rest/app-chat/conversations/new":
					call := generationCalls.Add(1)
					assertTestClearanceCookie(t, request, solverCalls.Load())
					if challengeStage == "generation" && call == 1 {
						writeTestChallenge(writer)
						return
					}
					writer.Header().Set("Content-Type", "application/json")
					_, _ = writer.Write([]byte(`{"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"users/test/generated/edit/image.jpg","progress":100}}}}`))
				default:
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()

			client := newTestMediaClient(server.URL)
			result, err := client.EditImage(context.Background(), "turn it blue", []string{testEditPNG}, "1:1")
			if err != nil {
				t.Fatal(err)
			}
			if len(result.URLs) != 1 || !strings.Contains(result.URLs[0], "/generated/edit/image.jpg") {
				t.Fatalf("urls = %#v", result.URLs)
			}
			wantGeneration := int32(1)
			if challengeStage == "generation" {
				wantGeneration = 2
			}
			if solverCalls.Load() != 2 || uploadCalls.Load() != 2 || generationCalls.Load() != wantGeneration {
				t.Fatalf("solver/upload/generation=%d/%d/%d, want 2/2/%d",
					solverCalls.Load(), uploadCalls.Load(), generationCalls.Load(), wantGeneration)
			}
		})
	}
}

func TestImageEditStructuredForbiddenDoesNotRetryOrRefresh(t *testing.T) {
	var solverCalls atomic.Int32
	var uploadCalls atomic.Int32
	var generationCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1":
			writeTestClearanceSolution(t, writer, solverCalls.Add(1))
		case "/http/upload-file-v2/direct":
			uploadCalls.Add(1)
			writeTestPolicyForbidden(writer)
		case "/rest/app-chat/conversations/new":
			generationCalls.Add(1)
			writeTestPolicyForbidden(writer)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := newTestMediaClient(server.URL)
	_, err := client.EditImage(context.Background(), "turn it blue", []string{testEditPNG}, "1:1")
	if err == nil {
		t.Fatal("expected structured forbidden")
	}
	if solverCalls.Load() != 1 || uploadCalls.Load() != 1 || generationCalls.Load() != 0 {
		t.Fatalf("solver/upload/generation=%d/%d/%d, want 1/1/0", solverCalls.Load(), uploadCalls.Load(), generationCalls.Load())
	}
}

func TestConfigFromEnvDefaultsOnDemandWhenSolverURLSet(t *testing.T) {
	cfg := Config{FlareSolverrURL: "http://127.0.0.1:8191"}.normalized()
	if cfg.ClearanceMode != ClearanceModeOnDemand {
		t.Fatalf("mode = %q", cfg.ClearanceMode)
	}
	cfg = Config{}.normalized()
	if cfg.ClearanceMode != ClearanceModeManual {
		t.Fatalf("empty config mode = %q", cfg.ClearanceMode)
	}
}

func newTestMediaClient(serverURL string) *Client {
	return NewClient(Config{
		BaseURL:            serverURL,
		FlareSolverrURL:    serverURL,
		ClearanceMode:      ClearanceModeFlareSolverr,
		StatsigMode:        StatsigModeManual,
		StatsigManualValue: base64.RawStdEncoding.EncodeToString(make([]byte, 70)),
		ImageTimeout:       5 * time.Second,
		ClearanceTimeout:   time.Second,
		ClearanceRefresh:   time.Hour,
	}, Credential{SSOToken: "test-sso"})
}

func writeTestClearanceSolution(t *testing.T, writer http.ResponseWriter, sequence int32) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(map[string]any{
		"status": "ok",
		"solution": map[string]any{
			"userAgent": "Mozilla/5.0 Chrome/146.0.0.0 Safari/537.36",
			"cookies":   []any{map[string]any{"name": "cf_clearance", "value": fmt.Sprintf("clearance-%d", sequence)}},
		},
	}); err != nil {
		t.Errorf("write Clearance solution: %v", err)
	}
}

func assertTestClearanceCookie(t *testing.T, request *http.Request, sequence int32) {
	t.Helper()
	want := fmt.Sprintf("cf_clearance=clearance-%d", sequence)
	if !strings.Contains(request.Header.Get("Cookie"), want) {
		t.Errorf("request %s did not use %s: %q", request.URL.Path, want, request.Header.Get("Cookie"))
	}
}

func writeTestChallenge(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "text/html")
	writer.WriteHeader(http.StatusForbidden)
	_, _ = writer.Write([]byte("<!doctype html><title>Just a moment...</title>"))
}

func writeTestPolicyForbidden(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusForbidden)
	_, _ = writer.Write([]byte(`{"error":{"code":"content-moderated","message":"rejected"}}`))
}
