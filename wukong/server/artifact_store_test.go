package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func newTestStore(t *testing.T, opts ArtifactStoreOptions) *ArtifactStore {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	s, err := NewArtifactStore(opts)
	if err != nil {
		t.Fatalf("NewArtifactStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

type countingFetcher struct {
	calls atomic.Int32
	data  []byte
	mime  string
	err   error
	delay time.Duration
}

func (f *countingFetcher) Fetch(_ context.Context, _ ArtifactRecord) (io.ReadCloser, string, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.err != nil {
		return nil, "", f.err
	}
	return io.NopCloser(bytes.NewReader(f.data)), f.mime, nil
}

func TestArtifactStoreRecordPutAndServeCached(t *testing.T) {
	s := newTestStore(t, ArtifactStoreOptions{})
	rec := ArtifactRecord{
		ID: "file_abc", Ext: "png", Provider: "chatgpt-web", AuthID: "chatgpt-web-1.json",
		Locator: ArtifactLocator{ConvID: "conv-1", FileID: "file_abc"},
	}
	if err := s.Record(rec); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := s.PutBytes("file_abc", []byte("PNGDATA"), "image/png"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !s.Cached("file_abc") {
		t.Fatal("expected cached")
	}
	if got := s.PublicURL(func(p string) string { return "http://gw" + p }, rec); got != "http://gw/files/file_abc.png" {
		t.Fatalf("PublicURL = %q", got)
	}

	fetcher := &countingFetcher{data: []byte("SHOULD NOT BE USED")}
	s.RegisterFetcher("chatgpt-web", fetcher)

	w := serveArtifact(s, "/files/file_abc.png", "")
	if w.Code != http.StatusOK || w.Body.String() != "PNGDATA" {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("Cache-Control = %q", cc)
	}
	if w.Header().Get("X-Artifact-Source") != "" {
		t.Fatal("cached hit must not be marked as upstream")
	}
	if fetcher.calls.Load() != 0 {
		t.Fatal("fetcher must not be called on cache hit")
	}
}

func TestArtifactStoreServeFallsBackToFetcherAndCaches(t *testing.T) {
	s := newTestStore(t, ArtifactStoreOptions{})
	if err := s.Record(ArtifactRecord{ID: "img1", Ext: "png", Provider: "grok-web", Locator: ArtifactLocator{URL: "https://assets.grok.com/x"}}); err != nil {
		t.Fatal(err)
	}
	fetcher := &countingFetcher{data: []byte("JPEGBYTES"), mime: "image/jpeg"}
	s.RegisterFetcher("grok-web", fetcher)

	w := serveArtifact(s, "/files/img1.png", "")
	if w.Code != http.StatusOK || w.Body.String() != "JPEGBYTES" {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Artifact-Source") != "upstream" {
		t.Fatal("first access must be marked upstream")
	}
	// mime 修正为上游返回值，扩展名跟着变
	rec, _ := s.Lookup("img1")
	if rec.Mime != "image/jpeg" || rec.Ext != "jpg" {
		t.Fatalf("record not updated from fetch: %+v", rec)
	}
	// 旧链接（.png）仍能命中同一个 id
	w2 := serveArtifact(s, "/files/img1.png", "")
	if w2.Code != http.StatusOK || w2.Header().Get("X-Artifact-Source") != "" {
		t.Fatalf("second access should be cached: status=%d src=%q", w2.Code, w2.Header().Get("X-Artifact-Source"))
	}
	if fetcher.calls.Load() != 1 {
		t.Fatalf("fetcher calls = %d, want 1", fetcher.calls.Load())
	}
}

func TestArtifactStoreServeErrors(t *testing.T) {
	s := newTestStore(t, ArtifactStoreOptions{})
	// 映射缺失 → 404
	if w := serveArtifact(s, "/files/unknown.png", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown: status=%d", w.Code)
	}
	// 非法名 → 404
	if w := serveArtifact(s, "/files/..%2f..%2fetc%2fpasswd", ""); w.Code != http.StatusNotFound {
		t.Fatalf("traversal: status=%d", w.Code)
	}
	// 映射存在但没有 fetcher → 502
	_ = s.Record(ArtifactRecord{ID: "nofetch", Ext: "png", Provider: "chatgpt-web"})
	if w := serveArtifact(s, "/files/nofetch.png", ""); w.Code != http.StatusBadGateway {
		t.Fatalf("no fetcher: status=%d", w.Code)
	}
	// fetcher 报错 → 502
	_ = s.Record(ArtifactRecord{ID: "fail", Ext: "png", Provider: "grok-web"})
	s.RegisterFetcher("grok-web", &countingFetcher{err: errors.New("credential missing")})
	w := serveArtifact(s, "/files/fail.png", "")
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "credential missing") {
		t.Fatalf("fetch error: status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestArtifactStoreConcurrentEnsureFetchesOnce(t *testing.T) {
	s := newTestStore(t, ArtifactStoreOptions{})
	_ = s.Record(ArtifactRecord{ID: "shared", Ext: "png", Provider: "grok-web"})
	fetcher := &countingFetcher{data: []byte("X"), mime: "image/png", delay: 50 * time.Millisecond}
	s.RegisterFetcher("grok-web", fetcher)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Ensure(context.Background(), "shared"); err != nil {
				t.Errorf("Ensure: %v", err)
			}
		}()
	}
	wg.Wait()
	if fetcher.calls.Load() != 1 {
		t.Fatalf("fetcher calls = %d, want 1 (singleflight)", fetcher.calls.Load())
	}
}

func TestArtifactStoreCleanupKeepsIndex(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, ArtifactStoreOptions{Dir: dir, MaxTotalBytes: 12})
	for i, id := range []string{"a", "b", "c"} {
		_ = s.Record(ArtifactRecord{ID: id, Ext: "png", Provider: "grok-web"})
		if err := s.PutBytes(id, []byte("123456"), "image/png"); err != nil {
			t.Fatal(err)
		}
		// 拉开 mtime，保证 a 最旧
		past := time.Now().Add(-time.Duration(3-i) * time.Hour)
		_ = os.Chtimes(filepath.Join(dir, id+".png"), past, past)
		s.cache.mu.Lock()
		s.cache.lastSeen[id] = past
		s.cache.mu.Unlock()
	}
	removed, freed := s.CleanupNow()
	if removed != 1 || freed != 6 {
		t.Fatalf("removed=%d freed=%d, want 1/6", removed, freed)
	}
	if s.Cached("a") {
		t.Fatal("oldest entry should be evicted")
	}
	if !s.Cached("b") || !s.Cached("c") {
		t.Fatal("newer entries must stay")
	}
	if _, ok := s.Lookup("a"); !ok {
		t.Fatal("index entry must survive cache eviction")
	}
	// 被清掉的产物访问时回源补回
	s.RegisterFetcher("grok-web", &countingFetcher{data: []byte("refetched"), mime: "image/png"})
	w := serveArtifact(s, "/files/a.png", "")
	if w.Code != http.StatusOK || w.Body.String() != "refetched" || w.Header().Get("X-Artifact-Source") != "upstream" {
		t.Fatalf("refetch: status=%d body=%q src=%q", w.Code, w.Body.String(), w.Header().Get("X-Artifact-Source"))
	}
}

func TestArtifactIndexPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, ArtifactStoreOptions{Dir: dir})
	rec := ArtifactRecord{
		ID: "persist", Ext: "pdf", Name: "report.pdf", Provider: "chatgpt-web", AuthID: "auth-1",
		Locator: ArtifactLocator{ConvID: "c", MessageID: "m", SandboxPath: "/mnt/data/report.pdf"},
	}
	if err := s.Record(rec); err != nil {
		t.Fatal(err)
	}
	// 幂等更新：补 AuthID，不改定位符
	if err := s.Record(ArtifactRecord{ID: "persist", Provider: "chatgpt-web", AuthID: "auth-2", Locator: ArtifactLocator{ConvID: "other"}}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	s2 := newTestStore(t, ArtifactStoreOptions{Dir: dir})
	got, ok := s2.Lookup("persist")
	if !ok {
		t.Fatal("record lost after reopen")
	}
	if got.AuthID != "auth-2" || got.Locator.ConvID != "c" || got.Name != "report.pdf" || got.Kind != ArtifactKindFile {
		t.Fatalf("record = %+v", got)
	}
	if s2.Cached("persist") {
		t.Fatal("no bytes were written; must not be cached")
	}
}

func TestArtifactIndexSkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	idx := filepath.Join(dir, "index.jsonl")
	good := `{"id":"ok","ext":"png","provider":"grok-web","created_at":"2026-09-07T00:00:00Z"}`
	if err := os.WriteFile(idx, []byte("not json\n"+good+"\n{\"id\":\"../bad\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t, ArtifactStoreOptions{Dir: dir, IndexPath: idx})
	if _, ok := s.Lookup("ok"); !ok {
		t.Fatal("valid line must load")
	}
	if _, ok := s.Lookup("../bad"); ok {
		t.Fatal("invalid id must be rejected")
	}
	if s.index.count() != 1 {
		t.Fatalf("count = %d", s.index.count())
	}
}

func TestArtifactCacheRejectsBadNames(t *testing.T) {
	s := newTestStore(t, ArtifactStoreOptions{})
	if err := s.Record(ArtifactRecord{ID: "../evil", Ext: "png", Provider: "grok-web"}); err == nil {
		t.Fatal("path traversal id must be rejected")
	}
	if _, err := s.cache.put("ok", "exe", strings.NewReader("x")); err == nil {
		t.Fatal("ext outside whitelist must be rejected")
	}
	if id, ext := splitArtifactName("abc.PNG"); id != "abc" || ext != "png" {
		t.Fatalf("split = %q/%q", id, ext)
	}
	if id, _ := splitArtifactName("a/b.png"); id != "" {
		t.Fatal("slash must be rejected")
	}
}

func TestArtifactIDHelpers(t *testing.T) {
	if got := ChatGPTImageArtifactID("file_00000000b8d881fd946c826feea9c8a7"); got != "file_00000000b8d881fd946c826feea9c8a7" {
		t.Fatalf("chatgpt id = %q", got)
	}
	if got := GrokAssetArtifactID("https://assets.grok.com/users/26a5f490-17c4-452d-a867-838270c39c11/generated/741C2AAE-b408-4e3f-aaec-522d701d3c76/image.jpg"); got != "741c2aae-b408-4e3f-aaec-522d701d3c76" {
		t.Fatalf("grok id = %q", got)
	}
	if got := GrokAssetArtifactID("https://assets.grok.com/no-uuid/here.jpg"); len(got) != 32 {
		t.Fatalf("grok fallback id = %q", got)
	}
	a := SandboxArtifactID("c", "m", "/mnt/data/a.pdf")
	b := SandboxArtifactID("c", "m", "/mnt/data/b.pdf")
	if a == b || len(a) != 32 {
		t.Fatalf("sandbox ids = %q / %q", a, b)
	}
	if ArtifactExtForName("https://x/y/clip.mp4?sig=1") != "mp4" || ArtifactExtForName("report.PDF") != "pdf" || ArtifactExtForName("x.exe") != "" {
		t.Fatal("ArtifactExtForName")
	}
	if ArtifactExtForMime("image/jpeg; charset=binary") != "jpg" || ArtifactExtForMime("video/mp4") != "mp4" || ArtifactExtForMime("application/x-unknown") != "" {
		t.Fatal("ArtifactExtForMime")
	}
}

func TestArtifactStoreServesRange(t *testing.T) {
	s := newTestStore(t, ArtifactStoreOptions{})
	_ = s.Record(ArtifactRecord{ID: "vid", Ext: "mp4", Provider: "grok-web"})
	_ = s.PutBytes("vid", []byte("0123456789"), "video/mp4")
	w := serveArtifact(s, "/files/vid.mp4", "bytes=2-5")
	if w.Code != http.StatusPartialContent || w.Body.String() != "2345" {
		t.Fatalf("range: status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestArtifactStoreContentDispositionForFiles(t *testing.T) {
	s := newTestStore(t, ArtifactStoreOptions{})
	_ = s.Record(ArtifactRecord{ID: "doc", Ext: "pdf", Name: "报告.pdf", Provider: "chatgpt-web"})
	_ = s.PutBytes("doc", []byte("%PDF"), "application/pdf")
	w := serveArtifact(s, "/files/doc.pdf", "")
	cd := w.Header().Get("Content-Disposition")
	if w.Code != http.StatusOK || !strings.HasPrefix(cd, "inline") || !strings.Contains(cd, "filename") {
		t.Fatalf("status=%d disposition=%q", w.Code, cd)
	}
}

// serveArtifact 走真实 gin 路由，验证 /files/:name 的注册方式。
func serveArtifact(s *ArtifactStore, target, rangeHeader string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterArtifactAndAdminRoutes(r, &ServerConfig{ImageDir: os.TempDir()}, nil, NewSessionManager(&ServerConfig{SessionTTLMinutes: 1}), nil, nil, nil, WithArtifactStore(s))
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
