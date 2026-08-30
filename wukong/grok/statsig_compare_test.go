package grok

import (
	"context"
	_ "embed"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

//go:embed testdata/botox_curves.json
var botoxCurvesFixture []byte

//go:embed testdata/botox_curves_live.json
var botoxCurvesLiveFixture []byte

//go:embed testdata/loading_x_anim.html
var loadingXAnimFixture []byte

func loadTestFrames(t *testing.T) [][][]int {
	t.Helper()
	frames, err := extractStatsigCurves([]byte(`{"curves":` + string(botoxCurvesFixture) + `}`))
	if err != nil {
		t.Fatal(err)
	}
	return frames
}

func loadLiveFrames(t *testing.T) [][][]int {
	t.Helper()
	frames, err := extractStatsigCurves([]byte(`{"curves":` + string(botoxCurvesLiveFixture) + `}`))
	if err != nil {
		t.Fatal(err)
	}
	return frames
}

// live 向量抓取自 2026-08-29 grok.com/imagine 的真实浏览器签名（钩住 crypto.subtle.digest
// 拿到喂给 SHA-256 的 hashInput，并与真实 x-statsig-id 的 hash 前 16 字节逐一对齐）。
// 这些值锁定当前 Grok 反爬算法：格式 METHOD!path!counter+"obfiowerehiring"+animationKey，
// meta 字节下标 5/39/28,36,41，Botox 曲线见 botox_curves_live.json。
func TestStatsigAnimationKeyMatchesLiveImagineSamples(t *testing.T) {
	frames := loadLiveFrames(t)
	samples := []struct {
		meta string
		key  string
	}{
		{"A1Hh9DGzAQ9m9EdXg8PAnvua6mDS9ZSCypbM82jY4v8j/WjW853LN5P/a03gUp50", "cd3d590f851eb851eb8504040f851eb851eb8500"},
		{"AWbgObdGEyo7WF4m/4yCqnaI4VLTqP4YLMykCx+UknImug7mi1RFXNRHqSDNx8oH", "2ad8e30eb851eb851eb8806147ae147ae14806147ae147ae1480eb851eb851eb8800"},
		{"iCWLhcmYw6nDpBUg9Xn6Eqk5f6cNJh2Gu5frGcy/EgoatowZWcNSqSUIH2JO+6WJ", "e7eebf101999999999999a01999999999999a100"},
		{"hd6xqr/3xCIgrwpODdiuN19HFCxsmbrC63CncZlu1U3iyUHhw4bMkefgZQ9DeJYa", "46413b100100"},
		{"tFojyacuwZzEO1fxWg3Yf8RZx+z5Fov5OS9US7E4OQBSvsu5Ux3YHJh8GQ1qBaWX", "33ebef100ccccccccccccd00ccccccccccccd100"},
		{"97LDdtUd5jyrDO7QunY8R1B0nSikzqyqa5LEL0GEcSu4EiRp0aqyPGFy17OVpyGc", "291c3a100100"},
		{"BrQQ7/ddw3Ta2spnSkjBc2raPojXC5Qcy9vv9tfoUxRthu51e7Na4EBKHVrsHLcU", "16c3eb0eb851eb851eb8806147ae147ae14806147ae147ae1480eb851eb851eb8800"},
		{"8GBKvuwwKsHgaf0qLqD6bw6lRF67Q+JP8Ep5+e9bW72mfu9DxPDSW3uzqWDWF14K", "c2ff8506b851eb851eb840e8f5c28f5c28f80e8f5c28f5c28f806b851eb851eb8400"},
		{"1IA+Xig9MX1sJmNtk4JNVczTOZCbPV2KGEWwFx0sTD9BQp0dTa6Lh2r6YrlrRsnc", "a8b2c0deb851eb851eb807d70a3d70a3d707d70a3d70a3d70deb851eb851eb800"},
		{"Lwve81yx0k72wG1/DqHX6jBuSncTqOEzYyrG6eZGpX2wv9RUaGmjbSld9Vm2LYhr", "e6e35b0b5c28f5c28f5c0b3333333333330b3333333333330b5c28f5c28f5c00"},
		{"TTk6RH+3eKVfY97AghCPcNZvys9ueD0ensrSsnb9mzJ0cZ86n7VrnDT8gef53MzN", "7938c40c7ae147ae147b09eb851eb851eb809eb851eb851eb80c7ae147ae147b00"},
		{"CGGapErn1K8NvnnusYg+k6b0TgBd1H93+frnarNyun++EXuxbhh3IYLCf169Cbj3", "4938370fd70a3d70a3d702b851eb851eb8602b851eb851eb860fd70a3d70a3d700"},
	}
	now := time.Date(2026, 8, 29, 6, 40, 0, 0, time.UTC)
	for _, sample := range samples {
		materials, err := buildStatsigMaterials(sample.meta, frames, now)
		if err != nil {
			t.Fatal(sample.meta[:8], err)
		}
		if materials.animationKey != sample.key {
			t.Fatalf("%s animation key = %s want %s", sample.meta[:8], materials.animationKey, sample.key)
		}
		signed, err := signStatsigIDWithTrace("POST", "/rest/app-chat/conversations/new", materials, now)
		if err != nil {
			t.Fatal(err)
		}
		if signed.Hash16 == "" || len(signed.Hash16) != 32 {
			t.Fatalf("%s hash16=%q", sample.meta[:8], signed.Hash16)
		}
		if !strings.Contains(signed.HashInput, sample.key) || !strings.HasPrefix(signed.HashInput, "POST!/rest/app-chat/conversations/new!") {
			t.Fatalf("%s hash input = %s", sample.meta[:8], signed.HashInput)
		}
		if signed.Indices != "5/39/28,36,41" || signed.IndexSource != "default" {
			t.Fatalf("indices=%s source=%s", signed.Indices, signed.IndexSource)
		}
	}

	const liveMeta = "CGGapErn1K8NvnnusYg+k6b0TgBd1H93+frnarNyun++EXuxbhh3IYLCf169Cbj3"
	liveNow := time.Unix(statsigEpoch+105061461, 0).UTC()
	liveMaterials, err := buildStatsigMaterials(liveMeta, frames, liveNow)
	if err != nil {
		t.Fatal(err)
	}
	vectors := []struct {
		method, path, hash16 string
	}{
		{"GET", "/rest/products", "556bb005fcf8ad920debb93757c97159"},
		{"POST", "/rest/models/imagine/overrides", "e288267500d295982d21c6579518dbc1"},
		{"POST", "/rest/media/imagine/quota_info", "d89dd16e416eb3baa12beb58a871ef94"},
		{"POST", "/rest/modes", "57560bc8853d9693e8f213513fd9a5ee"},
	}
	for _, v := range vectors {
		signed, err := signStatsigIDWithTrace(v.method, v.path, liveMaterials, liveNow)
		if err != nil {
			t.Fatal(err)
		}
		if signed.Hash16 != v.hash16 {
			t.Fatalf("%s %s hash16 = %s want %s", v.method, v.path, signed.Hash16, v.hash16)
		}
	}
}

func TestLoadStatsigCurvesFromSameHostChunk(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/imagine", "/":
			_, _ = writer.Write([]byte(`<html><head><meta name="grok-site-verification" content="knLu1M6WJzepfVkydy6LMV5Z4NDBwfi0DMjgsbL8i/Hmwd6hJw9FYH0z2w2EeZPV"/></head><script src="/_next/static/chunks/botox.js"></script></html>`))
		case "/_next/static/chunks/botox.js":
			hits++
			_, _ = writer.Write([]byte(`{"curves":` + string(botoxCurvesFixture) + `}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, StatsigMode: StatsigModeURL, StatsigSignerURL: "http://127.0.0.1:1/sign"}, Credential{SSOToken: "test-sso"})
	inspect, err := client.InspectStatsig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !inspect.Local || inspect.Source != "chunk" || inspect.AnimationKey == "" || hits == 0 {
		t.Fatalf("inspect=%#v hits=%d", inspect, hits)
	}
}

func TestLoadStatsigCurvesFromImportedChunk(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/imagine", "/":
			_, _ = writer.Write([]byte(`<html><head><meta name="grok-site-verification" content="knLu1M6WJzepfVkydy6LMV5Z4NDBwfi0DMjgsbL8i/Hmwd6hJw9FYH0z2w2EeZPV"/></head><script src="/_next/static/chunks/entry.js"></script></html>`))
		case "/_next/static/chunks/entry.js":
			_, _ = writer.Write([]byte(`import("/_next/static/chunks/botox.js")`))
		case "/_next/static/chunks/botox.js":
			_, _ = writer.Write([]byte(`{"curves":` + string(botoxCurvesFixture) + `}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, StatsigMode: StatsigModeURL, StatsigSignerURL: "http://127.0.0.1:1/sign"}, Credential{SSOToken: "test-sso"})
	inspect, err := client.InspectStatsig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !inspect.Local || inspect.Source != "chunk" || inspect.AnimationKey == "" {
		t.Fatalf("inspect=%#v", inspect)
	}
}

func escapeStatsigJSString(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return value
}

func TestExtractStatsigCurvesFromNextFlightScript(t *testing.T) {
	raw := string(botoxCurvesLiveFixture)
	html := `<script>self.__next_f.push([1,"84:[\"$\",\"$La2\",null,{\"curves\":` + escapeStatsigJSString(raw) + `,\"css_class\":\"r-18luo0\"}]\n"])</script>`
	frames, err := extractStatsigCurves([]byte(html))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateStatsigFrames(frames); err != nil {
		t.Fatal(err)
	}
	keyBytes, err := decodeStatsigMetaBytes("A1Hh9DGzAQ9m9EdXg8PAnvua6mDS9ZSCypbM82jY4v8j/WjW853LN5P/a03gUp50")
	if err != nil {
		t.Fatal(err)
	}
	key, err := statsigAnimationKey(keyBytes, frames)
	if err != nil {
		t.Fatal(err)
	}
	if key != "cd3d590f851eb851eb8504040f851eb851eb8500" {
		t.Fatalf("flight-extracted key = %s", key)
	}
}

func TestExtractStatsigCurvesFromSplitNextFlight(t *testing.T) {
	raw := string(botoxCurvesLiveFixture)
	mid := len(raw) / 2
	html := `<script>self.__next_f.push([1,"{\"curves\":` + escapeStatsigJSString(raw[:mid]) + `"])</script>` +
		`<script>self.__next_f.push([1,"` + escapeStatsigJSString(raw[mid:]) + `"])</script>`
	frames, err := extractStatsigCurves([]byte(html))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateStatsigFrames(frames); err != nil {
		t.Fatal(err)
	}
}

func TestExtractStatsigCurvesFromLoadingXAnimSVG(t *testing.T) {
	frames, err := extractStatsigCurves(loadingXAnimFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateStatsigFrames(frames); err != nil {
		t.Fatal(err)
	}
	keyBytes, err := decodeStatsigMetaBytes("qDERYWWMrY6+THhn9LY4akMGRnA3WD9M4tmxAd3lcFLn/wHI7GVSU/t8pcYurMIb")
	if err != nil {
		t.Fatal(err)
	}
	key, err := statsigAnimationKey(keyBytes, frames)
	if err != nil {
		t.Fatal(err)
	}
	if key != "99bba10947ae147ae14780cf5c28f5c28f60cf5c28f5c28f60947ae147ae147800" {
		t.Fatalf("svg-extracted key = %s", key)
	}
}

func TestLoadStatsigFramesFileAcceptsLiveJSON(t *testing.T) {
	frames, err := loadStatsigFramesFile("testdata/botox_curves_live.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateStatsigFrames(frames); err != nil {
		t.Fatal(err)
	}
}

func TestTrustedStatsigScriptURLRejectsForeignHost(t *testing.T) {
	if _, ok := trustedStatsigScriptURL("https://evil.example/_next/static/chunks/a.js", "https://grok.com/imagine"); ok {
		t.Fatal("foreign host must be rejected")
	}
	got, ok := trustedStatsigScriptURL("https://cdn.grok.com/_next/static/chunks/a.js", "https://grok.com/imagine")
	if !ok || got != "https://cdn.grok.com/_next/static/chunks/a.js" {
		t.Fatalf("cdn script = %q ok=%v", got, ok)
	}
}
