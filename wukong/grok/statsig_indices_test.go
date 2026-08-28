package grok

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractStatsigByteIndicesFromLiveHelperForm(t *testing.T) {
	src := []byte(`0x644f6370;var g=t[5]%4;W[5]%8;W[8]%8;var r=(W[12],16);var k=(W[18],16)*(W[11],16)*(W[46],16)`)
	got, ok := extractStatsigByteIndices(src)
	if !ok {
		t.Fatal("expected indices")
	}
	if got.String() != "5/12/18,11,46" {
		t.Fatalf("indices = %s", got.String())
	}
}

func TestExtractStatsigByteIndicesFromMod16Form(t *testing.T) {
	src := []byte(`group=t[5]%4; row=W[20]%16; time=(W[22]%16)*(W[19]%16)*(W[32]%16)`)
	got, ok := extractStatsigByteIndices(src)
	if !ok || got.String() != "5/20/22,19,32" {
		t.Fatalf("indices = %s ok=%v", got.String(), ok)
	}
}

func TestExtractStatsigByteIndicesFromFirstKnownForm(t *testing.T) {
	src := []byte(`t[5]%4; W[9]%16; (W[0]%16)*(W[18]%16)*(W[24]%16)`)
	got, ok := extractStatsigByteIndices(src)
	if !ok || got.String() != "5/9/0,18,24" {
		t.Fatalf("indices = %s ok=%v", got.String(), ok)
	}
}

func TestExtractStatsigByteIndicesIgnoresMod8Noise(t *testing.T) {
	src := []byte(`W[5]%8;W[8]%8;W[7]%40;t[5]%4;(W[12],16);(W[18],16)*(W[11],16)*(W[46],16)`)
	got, ok := extractStatsigByteIndices(src)
	if !ok || got.String() != "5/12/18,11,46" {
		t.Fatalf("indices = %s ok=%v", got.String(), ok)
	}
}

func TestExtractStatsigByteIndicesRequiresCluster(t *testing.T) {
	if _, ok := extractStatsigByteIndices([]byte(`W[12]%16; t[5]%4`)); ok {
		t.Fatal("incomplete source must not match")
	}
}

func TestExtractBotoxModuleIDAndChunkPath(t *testing.T) {
	entry := `let c=(o=async()=>(await e.A(4629918)).default(),async function(e,t){});e.s(["botoxSign",0,c],831076)`
	if got := extractBotoxModuleID(entry); got != "4629918" {
		t.Fatalf("module id = %q", got)
	}
	mapping := `146299180,s=>{};4629918,s=>{s.v(t=>Promise.all(["static/chunks/361dntxjo01k_.js"].map(t=>s.l(t))).then(()=>t(1645e3)))}`
	if got := extractModuleChunkPath(mapping, "4629918"); got != "static/chunks/361dntxjo01k_.js" {
		t.Fatalf("chunk path = %q", got)
	}
}

func TestResolveStatsigChunkRefPrefixesNext(t *testing.T) {
	got, ok := resolveStatsigChunkRef("static/chunks/signer.js", "https://grok.com/imagine")
	if !ok || got != "https://grok.com/_next/static/chunks/signer.js" {
		t.Fatalf("resolved = %q ok=%v", got, ok)
	}
}

func TestDiscoverStatsigIndicesFromLazySigner(t *testing.T) {
	frames := loadLiveFrames(t)
	meta := "N2TClog28dgNkB/DTx4dTDMVMyD7ciQ0TvBshBKifYegVKBZpoLYwhF1lGRrZlzF"
	metaBytes, err := decodeStatsigMetaBytes(meta)
	if err != nil {
		t.Fatal(err)
	}
	discovered := statsigByteIndices{SVG: 5, Row: 20, TimeA: 22, TimeB: 19, TimeC: 32, Source: "js"}
	wantKey, err := statsigAnimationKeyWith(metaBytes, frames, discovered)
	if err != nil {
		t.Fatal(err)
	}
	defaultKey, err := statsigAnimationKey(metaBytes, frames)
	if err != nil {
		t.Fatal(err)
	}
	if wantKey == defaultKey {
		t.Fatal("fixture indices must produce a different animation key than defaults")
	}

	html := `<html><head><meta name="grok-site-verification" content="` + meta + `"/>` +
		`<script src="/_next/static/chunks/entry.js"></script>` +
		`<script src="/_next/static/chunks/map.js"></script></head><body>` +
		`<script>self.__next_f.push([1,"{\"curves\":` + escapeStatsigJSString(string(botoxCurvesLiveFixture)) + `}"])</script>` +
		`</body></html>`
	var signerHits int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/imagine", "/":
			_, _ = writer.Write([]byte(html))
		case "/_next/static/chunks/entry.js":
			_, _ = writer.Write([]byte(`let c=(o=async()=>(await e.A(4629918)).default(),async function(e,t){});e.s(["botoxSign",0,c],831076)`))
		case "/_next/static/chunks/map.js":
			_, _ = writer.Write([]byte(`4629918,s=>{s.v(t=>Promise.all(["static/chunks/signer.js"].map(t=>s.l(t))).then(()=>t(1645e3)))}`))
		case "/_next/static/chunks/signer.js":
			signerHits++
			_, _ = writer.Write([]byte(`const epoch=0x644f6370;t[5]%4;W[5]%8;(W[20],16);(W[22],16)*(W[19],16)*(W[32],16)`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, StatsigMode: StatsigModeLocal}, Credential{SSOToken: "test-sso"})
	inspect, err := client.InspectStatsig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !inspect.Local || inspect.Source != "html" || inspect.IndexSource != "js" || inspect.Indices != "5/20/22,19,32" {
		t.Fatalf("inspect=%#v signerHits=%d", inspect, signerHits)
	}
	if inspect.AnimationKey != wantKey {
		t.Fatalf("key = %s want %s", inspect.AnimationKey, wantKey)
	}
	if signerHits == 0 {
		t.Fatal("lazy signer chunk was not fetched")
	}
}

func TestDiscoverStatsigIndicesFallsBackToDefaults(t *testing.T) {
	meta := "N2TClog28dgNkB/DTx4dTDMVMyD7ciQ0TvBshBKifYegVKBZpoLYwhF1lGRrZlzF"
	html := `<html><head><meta name="grok-site-verification" content="` + meta + `"/></head><body>` +
		`<script>self.__next_f.push([1,"{\"curves\":` + escapeStatsigJSString(string(botoxCurvesLiveFixture)) + `}"])</script>` +
		`</body></html>`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(html))
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, StatsigMode: StatsigModeLocal}, Credential{SSOToken: "test-sso"})
	inspect, err := client.InspectStatsig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inspect.IndexSource != "default" || inspect.Indices != defaultStatsigByteIndices().String() {
		t.Fatalf("inspect=%#v", inspect)
	}
	if !strings.HasPrefix(inspect.Source, "html") && inspect.Source != "html" {
		t.Fatalf("source=%q", inspect.Source)
	}
}
