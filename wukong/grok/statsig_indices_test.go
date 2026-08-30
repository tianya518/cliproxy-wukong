package grok

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
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

func TestExtractStatsigByteIndicesFromObfuscatedCallForm(t *testing.T) {
	src := []byte(`t[5]%4;let[a,G]=[s(W[39],16),s(s(W[36],16),s(W[41],16)),s(W[28],16)]`)
	got, ok := extractStatsigByteIndices(src)
	if !ok {
		t.Fatal("expected indices")
	}
	if got.SVG != 5 || got.Row != 39 {
		t.Fatalf("indices = %s", got.String())
	}
	times := []int{got.TimeA, got.TimeB, got.TimeC}
	sort.Ints(times)
	if times[0] != 28 || times[1] != 36 || times[2] != 41 {
		t.Fatalf("times = %s", got.String())
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
	if inspect.IndexSource != "default" || inspect.Indices != "5/39/28,36,41" {
		t.Fatalf("inspect=%#v", inspect)
	}
	if !strings.HasPrefix(inspect.Source, "html") && inspect.Source != "html" {
		t.Fatalf("source=%q", inspect.Source)
	}
}

func TestExtractStatsigScriptURLsSplitsCurveAndDiscoverBudgets(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("<html>")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&builder, `<script src="/_next/static/chunks/dummy-%02d.js"></script>`, i)
	}
	builder.WriteString(`<script src="/_next/static/chunks/entry.js"></script></html>`)
	page := "https://grok.com/imagine"
	body := []byte(builder.String())
	if got := extractStatsigScriptURLs(body, page); len(got) != statsigMaxScriptFetches {
		t.Fatalf("curve budget urls = %d", len(got))
	}
	all := extractStatsigScriptURLsN(body, page, statsigMaxScriptDiscover)
	if len(all) != 41 {
		t.Fatalf("discover urls = %d", len(all))
	}
	if !strings.HasSuffix(all[len(all)-1], "/entry.js") {
		t.Fatalf("last url = %s", all[len(all)-1])
	}
}

func TestDiscoverStatsigIndicesSkipsEarlyDummyScripts(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("<html><head>")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&builder, `<script src="/_next/static/chunks/dummy-%02d.js"></script>`, i)
	}
	builder.WriteString(`<script src="/_next/static/chunks/entry.js"></script>`)
	builder.WriteString(`<script src="/_next/static/chunks/map.js"></script></head></html>`)
	html := builder.String()

	var signerHits int
	var fetched []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fetched = append(fetched, request.URL.Path)
		switch request.URL.Path {
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

	discovery, ok := DiscoverStatsigByteIndices(context.Background(), []byte(html), server.URL+"/imagine", func(ctx context.Context, rawURL string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("status %d", resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	})
	if !ok || discovery.Source != "js" || discovery.Solution.String() != "5/20/19,22,32" {
		t.Fatalf("discovery=%#v ok=%v fetched=%v", discovery, ok, fetched)
	}
	if signerHits == 0 {
		t.Fatal("lazy signer chunk was not fetched")
	}
	if discovery.Fetches <= statsigMaxScriptFetches {
		t.Fatalf("expected discovery to walk past the curve budget, fetches=%d fetched=%v", discovery.Fetches, fetched)
	}
}
