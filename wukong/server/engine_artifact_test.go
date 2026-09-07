package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	sentinel "github.com/router-for-me/CLIProxyAPI/v7/wukong/sentinel"
)

func testAbsolute(p string) string { return "http://gw.example" + p }

func TestArtifactMarkdownUsesStoreLinksAndRecords(t *testing.T) {
	cfg := &ServerConfig{SessionTTLMinutes: 1, ImageDir: t.TempDir()}
	store := newTestStore(t, ArtifactStoreOptions{})
	e := NewEngine(cfg, nil, NewSessionManager(cfg))
	e.SetArtifactStore(store)

	env := ChatEnv{AuthID: "chatgpt-web-a.json", AbsoluteURL: testAbsolute}
	result := &sentinel.ChatResult{
		ExpectGeneratedImages: true,
		ImageFileIDs:          []string{"file_0001", "file_0002"},
		SandboxArtifacts: []sentinel.SandboxArtifact{
			{MessageID: "msg-1", SandboxPath: "/mnt/data/report.pdf", FileName: "report.pdf"},
		},
	}
	md := e.artifactMarkdown(env, nil, ChatCompletionRequest{}, result, "conv-1")

	if strings.Contains(md, "/api/image/proxy") || strings.Contains(md, "/api/pdf/proxy") {
		t.Fatalf("legacy proxy links must not appear when store is set:\n%s", md)
	}
	for _, want := range []string{
		"![Generated Image 1](http://gw.example/files/file_0001.png)",
		"![Generated Image 2](http://gw.example/files/file_0002.png)",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("missing %q in:\n%s", want, md)
		}
	}
	sandboxID := SandboxArtifactID("conv-1", "msg-1", "/mnt/data/report.pdf")
	if !strings.Contains(md, "[report.pdf](http://gw.example/files/"+sandboxID+".pdf)") {
		t.Fatalf("sandbox link missing in:\n%s", md)
	}

	// 映射已登记，且带上了凭证 ID 与定位符
	img, ok := store.Lookup("file_0001")
	if !ok || img.Provider != ArtifactProviderChatGPT || img.AuthID != "chatgpt-web-a.json" ||
		img.Locator.ConvID != "conv-1" || img.Locator.FileID != "file_0001" || img.Kind != ArtifactKindImage {
		t.Fatalf("image record = %+v ok=%v", img, ok)
	}
	pdf, ok := store.Lookup(sandboxID)
	if !ok || pdf.Kind != ArtifactKindFile || pdf.Name != "report.pdf" || pdf.Ext != "pdf" ||
		pdf.Locator.MessageID != "msg-1" || pdf.Locator.SandboxPath != "/mnt/data/report.pdf" {
		t.Fatalf("sandbox record = %+v ok=%v", pdf, ok)
	}
}

func TestArtifactMarkdownWithoutStoreKeepsLegacyLinks(t *testing.T) {
	cfg := &ServerConfig{SessionTTLMinutes: 1, ImageDir: t.TempDir()}
	e := NewEngine(cfg, nil, NewSessionManager(cfg))
	env := ChatEnv{AbsoluteURL: testAbsolute}
	result := &sentinel.ChatResult{ExpectGeneratedImages: true, ImageFileIDs: []string{"file_1"}}
	md := e.artifactMarkdown(env, nil, ChatCompletionRequest{}, result, "conv-1")
	if !strings.Contains(md, "http://gw.example/api/image/proxy?conv_id=conv-1&file_id=file_1") {
		t.Fatalf("legacy link expected:\n%s", md)
	}
}

func TestBuildArtifactConfigRecordsBeforeConvIDKnown(t *testing.T) {
	cfg := &ServerConfig{SessionTTLMinutes: 1, ImageDir: t.TempDir()}
	store := newTestStore(t, ArtifactStoreOptions{})
	e := NewEngine(cfg, nil, NewSessionManager(cfg))
	e.SetArtifactStore(store)

	env := ChatEnv{AuthID: "auth-x", AbsoluteURL: testAbsolute}
	entry := e.session.GetOrCreate("", "tok")
	acfg := e.buildArtifactConfig(env, entry, ChatCompletionRequest{}, "", nil)

	url := acfg.BuildImageURL("file_stream")
	if url != "http://gw.example/files/file_stream.png" {
		t.Fatalf("BuildImageURL = %q", url)
	}
	rec, ok := store.Lookup("file_stream")
	if !ok || rec.Locator.ConvID != "" || rec.AuthID != "auth-x" {
		t.Fatalf("early record = %+v ok=%v", rec, ok)
	}
	// 会话 ID 拿到后再登记一次，只补齐 ConvID，不换 ID / 链接
	url2 := e.imageArtifactURL(env, "auth-x", "conv-late", "file_stream")
	if url2 != url {
		t.Fatalf("url changed after conv known: %q vs %q", url2, url)
	}
	rec, _ = store.Lookup("file_stream")
	if rec.Locator.ConvID != "conv-late" {
		t.Fatalf("ConvID not backfilled: %+v", rec)
	}

	sb := acfg.BuildSandboxURL("m1", "/mnt/data/out.csv")
	if !strings.HasPrefix(sb, "http://gw.example/files/") || !strings.HasSuffix(sb, ".csv") {
		t.Fatalf("BuildSandboxURL = %q", sb)
	}
}

func TestLegacyProxyAliasServesFromStore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &ServerConfig{SessionTTLMinutes: 1, ImageDir: os.TempDir()}
	store := newTestStore(t, ArtifactStoreOptions{})
	_ = store.Record(ArtifactRecord{
		ID: "file_old", Ext: "png", Provider: ArtifactProviderChatGPT,
		Locator: ArtifactLocator{ConvID: "conv-old", FileID: "file_old"},
	})
	_ = store.PutBytes("file_old", []byte("IMG"), "image/png")
	sandboxID := SandboxArtifactID("conv-old", "m", "/mnt/data/a.pdf")
	_ = store.Record(ArtifactRecord{ID: sandboxID, Ext: "pdf", Name: "a.pdf", Provider: ArtifactProviderChatGPT,
		Locator: ArtifactLocator{ConvID: "conv-old", MessageID: "m", SandboxPath: "/mnt/data/a.pdf"}})
	_ = store.PutBytes(sandboxID, []byte("%PDF"), "application/pdf")

	r := gin.New()
	// 没有会话条目：以前会 404，现在映射命中直接从存储回
	RegisterArtifactAndAdminRoutes(r, cfg, nil, NewSessionManager(cfg), nil, nil, nil, WithArtifactStore(store))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/image/proxy?conv_id=conv-old&file_id=file_old", nil))
	if w.Code != http.StatusOK || w.Body.String() != "IMG" {
		t.Fatalf("image alias: status=%d body=%q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/pdf/proxy?conv_id=conv-old&msg_id=m&sandbox_path=%2Fmnt%2Fdata%2Fa.pdf", nil))
	if w.Code != http.StatusOK || w.Body.String() != "%PDF" {
		t.Fatalf("pdf alias: status=%d body=%q", w.Code, w.Body.String())
	}
	// 映射没有、会话也没有 → 仍是 404
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/image/proxy?conv_id=x&file_id=nope", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown alias: status=%d", w.Code)
	}
}
