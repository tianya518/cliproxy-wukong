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

func TestStatsigAnimationKeyMatchesLiveImagineSamples(t *testing.T) {
	frames := loadLiveFrames(t)
	samples := []struct {
		meta string
		key  string
	}{
		{"N2TClog28dgNkB/DTx4dTDMVMyD7ciQ0TvBshBKifYegVKBZpoLYwhF1lGRrZlzF", "a8cac410028f5c28f5c28f60028f5c28f5c28f6100"},
		{"HGDro4IESrKObEIv/f8BU3+o37iiOs/N1ppFMablmbJrXraa9TLPrblJB+AyVVXr", "555cf4100f5c28f5c28f5c00f5c28f5c28f5c100"},
	}
	now := time.Date(2026, 8, 26, 3, 20, 0, 0, time.UTC)
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
	keyBytes, err := decodeStatsigMetaBytes("N2TClog28dgNkB/DTx4dTDMVMyD7ciQ0TvBshBKifYegVKBZpoLYwhF1lGRrZlzF")
	if err != nil {
		t.Fatal(err)
	}
	key, err := statsigAnimationKey(keyBytes, frames)
	if err != nil {
		t.Fatal(err)
	}
	if key != "a8cac410028f5c28f5c28f60028f5c28f5c28f6100" {
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
	keyBytes, err := decodeStatsigMetaBytes("N2TClog28dgNkB/DTx4dTDMVMyD7ciQ0TvBshBKifYegVKBZpoLYwhF1lGRrZlzF")
	if err != nil {
		t.Fatal(err)
	}
	key, err := statsigAnimationKey(keyBytes, frames)
	if err != nil {
		t.Fatal(err)
	}
	if key != "a8cac410028f5c28f5c28f60028f5c28f5c28f6100" {
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
