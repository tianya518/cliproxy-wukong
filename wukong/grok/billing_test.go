package grok

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// 按线上 2026-09-04 GetGrokCreditsConfig 的真实回包结构手工编一份 GrokCreditsConfig。
func encodeCreditsConfigFixture() []byte {
	ts := func(sec int64) []byte {
		return protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), uint64(sec))
	}
	cent := func(val int64) []byte {
		if val == 0 {
			return nil
		}
		return protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), uint64(val))
	}
	var cfg []byte
	cfg = protowire.AppendFixed32(protowire.AppendTag(cfg, 1, protowire.Fixed32Type), math.Float32bits(6))
	cfg = protowire.AppendBytes(protowire.AppendTag(cfg, 2, protowire.BytesType), cent(0))
	cfg = protowire.AppendBytes(protowire.AppendTag(cfg, 3, protowire.BytesType), cent(0))
	cfg = protowire.AppendBytes(protowire.AppendTag(cfg, 4, protowire.BytesType), ts(1788155959))
	cfg = protowire.AppendBytes(protowire.AppendTag(cfg, 5, protowire.BytesType), ts(1788760759))
	var product []byte
	product = protowire.AppendVarint(protowire.AppendTag(product, 1, protowire.VarintType), 5)
	product = protowire.AppendFixed32(protowire.AppendTag(product, 2, protowire.Fixed32Type), math.Float32bits(6))
	cfg = protowire.AppendBytes(protowire.AppendTag(cfg, 7, protowire.BytesType), product)
	var period []byte
	period = protowire.AppendVarint(protowire.AppendTag(period, 1, protowire.VarintType), 2)
	period = protowire.AppendBytes(protowire.AppendTag(period, 2, protowire.BytesType), ts(1788155959))
	period = protowire.AppendBytes(protowire.AppendTag(period, 3, protowire.BytesType), ts(1788760759))
	cfg = protowire.AppendBytes(protowire.AppendTag(cfg, 8, protowire.BytesType), period)
	cfg = protowire.AppendVarint(protowire.AppendTag(cfg, 11, protowire.VarintType), 1)
	cfg = protowire.AppendBytes(protowire.AppendTag(cfg, 12, protowire.BytesType), cent(1250))
	cfg = protowire.AppendVarint(protowire.AppendTag(cfg, 13, protowire.VarintType), 1)
	return protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), cfg)
}

func encodeRemainingResetsFixture() []byte {
	ts := func(sec int64) []byte {
		return protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), uint64(sec))
	}
	var token []byte
	token = protowire.AppendString(protowire.AppendTag(token, 10, protowire.BytesType), "restok_vpYDqo")
	token = protowire.AppendBytes(protowire.AppendTag(token, 20, protowire.BytesType), ts(1786560540))
	token = protowire.AppendBytes(protowire.AppendTag(token, 30, protowire.BytesType), ts(1789238940))
	return protowire.AppendBytes(protowire.AppendTag(nil, 10, protowire.BytesType), token)
}

func grpcWebFrame(flag byte, payload []byte) []byte {
	frame := make([]byte, 5, 5+len(payload))
	frame[0] = flag
	binary.BigEndian.PutUint32(frame[1:], uint32(len(payload)))
	return append(frame, payload...)
}

func grpcWebOK(message []byte) []byte {
	return append(grpcWebFrame(0x00, message), grpcWebFrame(0x80, []byte("grpc-status:0\r\n"))...)
}

func newBillingTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cfg := Config{
		BaseURL:            server.URL,
		StatsigMode:        StatsigModeManual,
		StatsigManualValue: base64.RawStdEncoding.EncodeToString(make([]byte, 70)),
	}
	return NewClient(cfg, Credential{SSOToken: "test-sso"})
}

func TestSyncBillingDecodesGrpcWebFrames(t *testing.T) {
	var seen []string
	client := newBillingTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		seen = append(seen, request.URL.Path)
		if got := request.Header.Get("Content-Type"); got != "application/grpc-web+proto" {
			t.Errorf("content-type = %q", got)
		}
		if request.Header.Get("X-Grpc-Web") != "1" {
			t.Errorf("missing x-grpc-web header")
		}
		if !strings.Contains(request.Header.Get("Cookie"), "sso=test-sso") {
			t.Errorf("cookie = %q", request.Header.Get("Cookie"))
		}
		// 空请求必须是 5 字节帧头，服务端把 JSON 当帧解会报 compression flag 123。
		if len(body) != 5 || body[0] != 0 || binary.BigEndian.Uint32(body[1:]) != 0 {
			t.Errorf("request frame = %x", body)
		}
		writer.Header().Set("Content-Type", "application/grpc-web+proto")
		switch request.URL.Path {
		case billingCreditsConfigPath:
			_, _ = writer.Write(grpcWebOK(encodeCreditsConfigFixture()))
		case billingRemainingResetsPath:
			_, _ = writer.Write(grpcWebOK(encodeRemainingResetsFixture()))
		default:
			http.NotFound(writer, request)
		}
	})

	snapshot, err := client.SyncBilling(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != billingCreditsConfigPath || seen[1] != billingRemainingResetsPath {
		t.Fatalf("calls = %v", seen)
	}
	if snapshot.UsagePercent != 6 || snapshot.PeriodType != "weekly" || !snapshot.UnifiedBilling {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.PeriodEnd == nil || snapshot.PeriodEnd.Unix() != 1788760759 || snapshot.PeriodStart == nil || snapshot.PeriodStart.Unix() != 1788155959 {
		t.Fatalf("period = %v .. %v", snapshot.PeriodStart, snapshot.PeriodEnd)
	}
	if snapshot.PrepaidBalanceCents != 1250 || snapshot.OnDemandCapCents != 0 || snapshot.OnDemandUsedCents != 0 {
		t.Fatalf("cents = %+v", snapshot)
	}
	if len(snapshot.Products) != 1 || snapshot.Products[0].Product != "imagine" || snapshot.Products[0].UsagePercent != 6 {
		t.Fatalf("products = %+v", snapshot.Products)
	}
	if snapshot.ResetsError != "" || len(snapshot.Resets) != 1 {
		t.Fatalf("resets = %+v err=%q", snapshot.Resets, snapshot.ResetsError)
	}
	reset := snapshot.Resets[0]
	if reset.TokenID != "restok_vpYDqo" || reset.ValidUntil == nil || reset.ValidUntil.Unix() != 1789238940 || reset.ValidFrom == nil || reset.ValidFrom.Unix() != 1786560540 {
		t.Fatalf("reset = %+v", reset)
	}
}

// 重置券接口挂了不该拖垮整份订阅额度。
func TestSyncBillingKeepsConfigWhenResetsFail(t *testing.T) {
	client := newBillingTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/grpc-web+proto")
		if request.URL.Path == billingCreditsConfigPath {
			_, _ = writer.Write(grpcWebOK(encodeCreditsConfigFixture()))
			return
		}
		// trailers-only 错误：状态直接在 HTTP 头里。
		writer.Header().Set("Grpc-Status", "13")
		writer.Header().Set("Grpc-Message", "Unexpected%20EOF%20decoding%20stream.")
		writer.WriteHeader(http.StatusOK)
	})

	snapshot, err := client.SyncBilling(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.UsagePercent != 6 || len(snapshot.Resets) != 0 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if !strings.Contains(snapshot.ResetsError, "13") || !strings.Contains(snapshot.ResetsError, "Unexpected EOF decoding stream.") {
		t.Fatalf("resets_error = %q", snapshot.ResetsError)
	}
}

func TestSyncBillingSurfacesTrailerStatusAndAuthErrors(t *testing.T) {
	trailerErr := newBillingTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/grpc-web+proto")
		_, _ = writer.Write(grpcWebFrame(0x80, []byte("grpc-status:16\r\ngrpc-message:unauthenticated\r\n")))
	})
	if _, err := trailerErr.SyncBilling(context.Background()); err == nil || !strings.Contains(err.Error(), "16") {
		t.Fatalf("expected trailer status error, got %v", err)
	}

	unauthorized := newBillingTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
	})
	if _, err := unauthorized.SyncBilling(context.Background()); err != ErrUnauthorized {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}

	notFound := newBillingTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		http.NotFound(writer, request)
	})
	if _, err := notFound.SyncBilling(context.Background()); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected 404 error, got %v", err)
	}
}

// QuotaFor 里订阅额度是附加信息：拿不到只记 billing_error，滚动窗口照常返回。
func TestQuotaForAttachesBillingBestEffort(t *testing.T) {
	handler := func(billingOK bool) http.HandlerFunc {
		return func(writer http.ResponseWriter, request *http.Request) {
			switch request.URL.Path {
			case "/rest/rate-limits":
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`{"windowSizeSeconds":7200,"remainingQueries":150,"totalQueries":150}`))
			case "/rest/media/imagine/quota_info":
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`{"image":{"available":true}}`))
			case billingCreditsConfigPath:
				if !billingOK {
					http.Error(writer, "nope", http.StatusBadGateway)
					return
				}
				writer.Header().Set("Content-Type", "application/grpc-web+proto")
				_, _ = writer.Write(grpcWebOK(encodeCreditsConfigFixture()))
			case billingRemainingResetsPath:
				writer.Header().Set("Content-Type", "application/grpc-web+proto")
				_, _ = writer.Write(grpcWebOK(nil))
			default:
				http.NotFound(writer, request)
			}
		}
	}
	for _, billingOK := range []bool{true, false} {
		server := httptest.NewServer(handler(billingOK))
		cfg := Config{
			BaseURL:            server.URL,
			StatsigMode:        StatsigModeManual,
			StatsigManualValue: base64.RawStdEncoding.EncodeToString(make([]byte, 70)),
		}
		result := QuotaFor(context.Background(), cfg, Credential{Name: "acc", SSOToken: "test-sso"})
		server.Close()
		if result.Error != "" || len(result.Windows) != 3 {
			t.Fatalf("billingOK=%v result = %+v", billingOK, result)
		}
		if billingOK {
			if result.Billing == nil || result.Billing.UsagePercent != 6 || result.BillingError != "" || len(result.Billing.Resets) != 0 {
				t.Fatalf("billing = %+v err=%q", result.Billing, result.BillingError)
			}
			continue
		}
		if result.Billing != nil || !strings.Contains(result.BillingError, "502") {
			t.Fatalf("expected billing_error with 502, got billing=%+v err=%q", result.Billing, result.BillingError)
		}
	}
}

func TestParseGrpcWebFramesRejectsGarbage(t *testing.T) {
	if _, err := parseGrpcWebFrames([]byte("<html>")); err == nil {
		t.Fatal("html body must not parse as frames")
	}
	if _, err := parseGrpcWebFrames(grpcWebFrame(0x80, []byte("grpc-status:0\r\n"))); err == nil {
		t.Fatal("trailer-only body without data frame must error")
	}
	oversize := []byte{0, 0xff, 0xff, 0xff, 0xff, 1}
	if _, err := parseGrpcWebFrames(oversize); err == nil {
		t.Fatal("frame length beyond body must error")
	}
	if msg, err := parseGrpcWebFrames(grpcWebOK([]byte{0x08, 0x01})); err != nil || string(msg) != "\x08\x01" {
		t.Fatalf("msg=%x err=%v", msg, err)
	}
}

func TestParseTimestampAndCentHelpers(t *testing.T) {
	if parseTimestamp(nil) != nil {
		t.Fatal("empty timestamp should be nil")
	}
	raw := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 1788760759)
	raw = protowire.AppendVarint(protowire.AppendTag(raw, 2, protowire.VarintType), 500_000_000)
	got := parseTimestamp(raw)
	if got == nil || !got.Equal(time.Unix(1788760759, 500_000_000)) {
		t.Fatalf("timestamp = %v", got)
	}
	if parseCent(nil) != 0 || parseCent(protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 1250)) != 1250 {
		t.Fatal("cent decode mismatch")
	}
	if billingProductName(5) != "imagine" || billingProductName(42) != "product_42" || billingPeriodTypeName(2) != "weekly" {
		t.Fatal("enum name mapping mismatch")
	}
}
