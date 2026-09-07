package server

// artifact_store.go —— 产物存储 = 持久映射（artifact_index.go）+ 磁盘缓存（artifact_cache.go）。
//
// 对外只有一种链接：<ARTIFACT_BASE_URL>/files/<id>.<ext>。取回三级：
//  1. 缓存命中 → 直接回（immutable、支持 Range）；
//  2. 缓存缺失、映射命中 → 用映射里的凭证 ID 找当前有效凭证，经 provider fetcher 回源，写回缓存再回；
//  3. 映射也没有 → 404。
// 生成侧先 Record 再（尽量）Put，链接在 Record 之后就可以发出。
// fetcher 由 cliproxy 胶水层注册（需要凭证池），本包只定义接口。

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/singleflight"
)

// 产物来源 provider 标识，与 cliproxy 侧的 provider key 一致。
const (
	ArtifactProviderChatGPT = "chatgpt-web"
	ArtifactProviderGrok    = "grok-web"
)

// 包内简写。
const chatGPTArtifactProvider = ArtifactProviderChatGPT

// ArtifactFetcher 按映射记录去上游取字节。由各 provider 实现并注册。
type ArtifactFetcher interface {
	Fetch(ctx context.Context, rec ArtifactRecord) (body io.ReadCloser, mimeType string, err error)
}

// ArtifactFetcherFunc 函数式适配。
type ArtifactFetcherFunc func(ctx context.Context, rec ArtifactRecord) (io.ReadCloser, string, error)

func (f ArtifactFetcherFunc) Fetch(ctx context.Context, rec ArtifactRecord) (io.ReadCloser, string, error) {
	return f(ctx, rec)
}

var (
	// ErrArtifactNotFound 映射里没有这个 ID。
	ErrArtifactNotFound = errors.New("artifact not found")
	// ErrArtifactUnavailable 映射存在但取不回来（凭证缺失、上游拒绝、没有 fetcher）。
	ErrArtifactUnavailable = errors.New("artifact unavailable")
)

// ArtifactStoreOptions 构造参数，零值取默认。
type ArtifactStoreOptions struct {
	Dir           string        // 缓存目录，默认 artifacts
	IndexPath     string        // 映射表，默认 <Dir>/index.jsonl
	PublicPath    string        // 对外路径前缀，默认 /files
	MaxTotalBytes int64         // 缓存上限，<=0 不限
	MaxAge        time.Duration // 缓存按时长清理，<=0 不限
	FetchTimeout  time.Duration // 回源单次超时，默认 60s
}

// ArtifactStore 产物存储。
type ArtifactStore struct {
	index        *artifactIndex
	cache        *artifactCache
	publicPath   string
	fetchTimeout time.Duration

	fetchMu  sync.RWMutex
	fetchers map[string]ArtifactFetcher
	flight   singleflight.Group

	cleanupOnce sync.Once
	cleanupDone chan struct{}
}

// NewArtifactStore 打开（或创建）映射表与缓存目录。
func NewArtifactStore(opts ArtifactStoreOptions) (*ArtifactStore, error) {
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		dir = "artifacts"
	}
	indexPath := strings.TrimSpace(opts.IndexPath)
	if indexPath == "" {
		indexPath = filepath.Join(dir, "index.jsonl")
	}
	publicPath := "/" + strings.Trim(strings.TrimSpace(opts.PublicPath), "/")
	if publicPath == "/" {
		publicPath = "/files"
	}
	fetchTimeout := opts.FetchTimeout
	if fetchTimeout <= 0 {
		fetchTimeout = 60 * time.Second
	}
	cache, err := openArtifactCache(dir, opts.MaxTotalBytes, opts.MaxAge)
	if err != nil {
		return nil, err
	}
	index, err := openArtifactIndex(indexPath)
	if err != nil {
		return nil, err
	}
	return &ArtifactStore{
		index:        index,
		cache:        cache,
		publicPath:   publicPath,
		fetchTimeout: fetchTimeout,
		fetchers:     make(map[string]ArtifactFetcher),
		cleanupDone:  make(chan struct{}),
	}, nil
}

// NewArtifactStoreFromConfig 按 ServerConfig 的 ARTIFACT_* 配置构造。
func NewArtifactStoreFromConfig(cfg *ServerConfig) (*ArtifactStore, error) {
	if cfg == nil {
		return NewArtifactStore(ArtifactStoreOptions{})
	}
	var maxTotal int64
	if cfg.ArtifactMaxTotalMB > 0 {
		maxTotal = int64(cfg.ArtifactMaxTotalMB) << 20
	}
	var maxAge time.Duration
	if cfg.ArtifactMaxAgeDays > 0 {
		maxAge = time.Duration(cfg.ArtifactMaxAgeDays) * 24 * time.Hour
	}
	return NewArtifactStore(ArtifactStoreOptions{
		Dir:           cfg.ArtifactDir,
		IndexPath:     cfg.ArtifactIndexPath,
		PublicPath:    cfg.ArtifactPublicPath,
		MaxTotalBytes: maxTotal,
		MaxAge:        maxAge,
		FetchTimeout:  time.Duration(cfg.ArtifactFetchTimeoutSec) * time.Second,
	})
}

// PublicPath 对外路径前缀（如 /files）。
func (s *ArtifactStore) PublicPath() string { return s.publicPath }

// RegisterFetcher 注册某 provider 的回源实现。
func (s *ArtifactStore) RegisterFetcher(provider string, f ArtifactFetcher) {
	if s == nil || f == nil {
		return
	}
	s.fetchMu.Lock()
	s.fetchers[strings.ToLower(strings.TrimSpace(provider))] = f
	s.fetchMu.Unlock()
}

func (s *ArtifactStore) fetcherFor(provider string) (ArtifactFetcher, bool) {
	s.fetchMu.RLock()
	defer s.fetchMu.RUnlock()
	f, ok := s.fetchers[strings.ToLower(strings.TrimSpace(provider))]
	return f, ok
}

// Record 登记映射（幂等）。链接必须在 Record 之后才发出。
func (s *ArtifactStore) Record(rec ArtifactRecord) error {
	if s == nil {
		return errors.New("artifact store not configured")
	}
	rec.ID = strings.TrimSpace(rec.ID)
	rec.Provider = strings.ToLower(strings.TrimSpace(rec.Provider))
	if rec.ID == "" || rec.Provider == "" {
		return fmt.Errorf("artifact record: id and provider are required")
	}
	if ext := NormalizeArtifactExt(rec.Ext); ext != "" {
		rec.Ext = ext
	} else if rec.Mime != "" {
		rec.Ext = ArtifactExtForMime(rec.Mime)
	}
	if rec.Ext == "" {
		rec.Ext = "bin"
	}
	if rec.Kind == "" {
		rec.Kind = artifactKindForExt(rec.Ext)
	}
	return s.index.put(rec)
}

// Put 把字节写进缓存。mimeType 非空时顺带修正映射里的 mime / ext。
func (s *ArtifactStore) Put(id string, r io.Reader, mimeType string) error {
	if s == nil {
		return errors.New("artifact store not configured")
	}
	rec, ok := s.index.get(id)
	if !ok {
		return fmt.Errorf("artifact %q: %w (record first)", id, ErrArtifactNotFound)
	}
	ext := rec.Ext
	mimeType = strings.TrimSpace(mimeType)
	if mimeType != "" {
		if mediaType, _, err := mime.ParseMediaType(mimeType); err == nil {
			mimeType = mediaType
		}
		if byMime := ArtifactExtForMime(mimeType); byMime != "" && byMime != "bin" {
			ext = byMime
		}
	}
	if _, err := s.cache.put(id, ext, r); err != nil {
		return err
	}
	if (mimeType != "" && mimeType != rec.Mime) || ext != rec.Ext {
		update := ArtifactRecord{ID: id, Provider: rec.Provider, Ext: ext, Mime: mimeType}
		if err := s.index.put(update); err != nil {
			log.Printf("[artifact] 更新 %s 的 mime/ext 失败: %v", id, err)
		}
	}
	return nil
}

// PutBytes 是 Put 的字节切片便捷版。
func (s *ArtifactStore) PutBytes(id string, data []byte, mimeType string) error {
	return s.Put(id, bytes.NewReader(data), mimeType)
}

// Lookup 查映射。
func (s *ArtifactStore) Lookup(id string) (ArtifactRecord, bool) {
	if s == nil {
		return ArtifactRecord{}, false
	}
	return s.index.get(strings.TrimSpace(id))
}

// Cached 报告 id 的字节是否已在缓存里。
func (s *ArtifactStore) Cached(id string) bool {
	if s == nil {
		return false
	}
	_, ok := s.cache.has(id)
	return ok
}

// PublicURL 生成对外链接：base(publicPath/<id>.<ext>)。base 为空时返回相对路径。
func (s *ArtifactStore) PublicURL(base func(string) string, rec ArtifactRecord) string {
	ext := rec.Ext
	if ext == "" {
		ext = "bin"
	}
	rel := s.publicPath + "/" + rec.ID + "." + ext
	if base == nil {
		return rel
	}
	return base(rel)
}

// PublicURLByID 用映射里的记录生成链接；映射缺失返回空。
func (s *ArtifactStore) PublicURLByID(base func(string) string, id string) string {
	rec, ok := s.Lookup(id)
	if !ok {
		return ""
	}
	return s.PublicURL(base, rec)
}

// Ensure 确保 id 的字节在缓存里：缺失则回源补回。并发同 id 只打一次上游。
func (s *ArtifactStore) Ensure(ctx context.Context, id string) (ArtifactRecord, error) {
	if s == nil {
		return ArtifactRecord{}, errors.New("artifact store not configured")
	}
	rec, ok := s.index.get(id)
	if !ok {
		return ArtifactRecord{}, ErrArtifactNotFound
	}
	if _, cached := s.cache.has(id); cached {
		return rec, nil
	}
	v, err, _ := s.flight.Do(id, func() (any, error) {
		if _, cached := s.cache.has(id); cached {
			return rec, nil
		}
		return s.fetchIntoCache(ctx, rec)
	})
	if err != nil {
		return rec, err
	}
	if r, ok := v.(ArtifactRecord); ok {
		return r, nil
	}
	return rec, nil
}

func (s *ArtifactStore) fetchIntoCache(ctx context.Context, rec ArtifactRecord) (ArtifactRecord, error) {
	f, ok := s.fetcherFor(rec.Provider)
	if !ok {
		return rec, fmt.Errorf("%w: no fetcher for provider %q", ErrArtifactUnavailable, rec.Provider)
	}
	fctx, cancel := context.WithTimeout(ctx, s.fetchTimeout)
	defer cancel()
	body, mimeType, err := f.Fetch(fctx, rec)
	if err != nil {
		return rec, fmt.Errorf("%w: %v", ErrArtifactUnavailable, err)
	}
	defer body.Close()
	if err := s.Put(rec.ID, body, mimeType); err != nil {
		return rec, fmt.Errorf("%w: cache write: %v", ErrArtifactUnavailable, err)
	}
	updated, _ := s.index.get(rec.ID)
	return updated, nil
}

// Open 读产物字节：缓存命中直接开，否则回源补回后再开。
func (s *ArtifactStore) Open(ctx context.Context, id string) (io.ReadCloser, ArtifactRecord, error) {
	rec, err := s.Ensure(ctx, id)
	if err != nil {
		return nil, rec, err
	}
	f, _, err := s.cache.open(id)
	if err != nil {
		return nil, rec, fmt.Errorf("%w: %v", ErrArtifactUnavailable, err)
	}
	return f, rec, nil
}

// Serve 处理 GET <publicPath>/<name>。
func (s *ArtifactStore) Serve(c *gin.Context, name string) {
	id, _ := splitArtifactName(strings.TrimSpace(name))
	if id == "" {
		c.String(http.StatusNotFound, "not found")
		return
	}
	rec, ok := s.index.get(id)
	if !ok {
		c.String(http.StatusNotFound, "not found")
		return
	}
	_, wasCached := s.cache.has(id)
	rec, err := s.Ensure(c.Request.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, ErrArtifactNotFound):
			c.String(http.StatusNotFound, "not found")
		default:
			c.String(http.StatusBadGateway, "artifact unavailable: %v", err)
		}
		return
	}
	f, info, err := s.cache.open(id)
	if err != nil {
		c.String(http.StatusBadGateway, "artifact unavailable: %v", err)
		return
	}
	defer f.Close()

	h := c.Writer.Header()
	h.Set("Content-Type", artifactMimeFor(rec))
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("X-Content-Type-Options", "nosniff")
	if !wasCached {
		h.Set("X-Artifact-Source", "upstream")
	}
	if rec.Kind == ArtifactKindFile {
		fileName := rec.Name
		if fileName == "" {
			fileName = rec.ID + "." + rec.Ext
		}
		h.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": fileName}))
	}
	http.ServeContent(c.Writer, c.Request, info.Name(), info.ModTime(), f)
}

// StartCleanup 启动后台缓存清理，ctx 结束即停止。
func (s *ArtifactStore) StartCleanup(ctx context.Context) {
	if s == nil {
		return
	}
	s.cleanupOnce.Do(func() {
		go func() {
			<-ctx.Done()
			close(s.cleanupDone)
		}()
		go s.cache.runCleanup(s.cleanupDone, 10*time.Minute)
	})
}

// CleanupNow 立刻做一次清理（测试与运维用）。
func (s *ArtifactStore) CleanupNow() (removed int, freed int64) {
	if s == nil {
		return 0, 0
	}
	return s.cache.cleanup(time.Now())
}

// Close 关闭映射表文件。
func (s *ArtifactStore) Close() error {
	if s == nil {
		return nil
	}
	return s.index.close()
}

// ─── ID / mime 辅助 ──────────────────────────────────────────────────────────

var nonArtifactIDChar = regexp.MustCompile(`[^A-Za-z0-9_-]`)
var uuidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

func shortSHA1(parts ...string) string {
	h := sha1.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// ChatGPTImageArtifactID 生图 file_id 直接作为主键（全局唯一）。
func ChatGPTImageArtifactID(fileID string) string {
	id := nonArtifactIDChar.ReplaceAllString(strings.TrimSpace(fileID), "_")
	if id == "" || len(id) > 128 {
		return shortSHA1("chatgpt-web", "image", fileID)
	}
	return id
}

// SandboxArtifactID 沙箱文件按 会话 + 消息 + 路径 哈希。
func SandboxArtifactID(convID, messageID, sandboxPath string) string {
	return shortSHA1("chatgpt-web", "sandbox", convID, messageID, sandboxPath)
}

// GrokAssetArtifactID 取资源 URL 里的 UUID 段，取不到则哈希整个 URL。
func GrokAssetArtifactID(rawURL string) string {
	if m := uuidRe.FindAllString(rawURL, -1); len(m) > 0 {
		return strings.ToLower(m[len(m)-1])
	}
	return shortSHA1("grok-web", rawURL)
}

// ArtifactExtForName 从文件名 / URL 路径取扩展名（白名单内），否则空。
func ArtifactExtForName(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.IndexAny(name, "?#"); i >= 0 {
		name = name[:i]
	}
	return NormalizeArtifactExt(path.Ext(name))
}

// ArtifactExtForMime 从 MIME 推扩展名，未知返回空。
func ArtifactExtForMime(mimeType string) string {
	mt := strings.ToLower(strings.TrimSpace(mimeType))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	switch mt {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "video/mp4":
		return "mp4"
	case "video/webm":
		return "webm"
	case "application/pdf":
		return "pdf"
	case "text/plain":
		return "txt"
	case "text/csv":
		return "csv"
	case "text/markdown":
		return "md"
	case "application/json":
		return "json"
	case "application/zip":
		return "zip"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return "docx"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return "xlsx"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return "pptx"
	}
	return ""
}

func artifactKindForExt(ext string) string {
	switch NormalizeArtifactExt(ext) {
	case "png", "jpg", "webp", "gif":
		return ArtifactKindImage
	case "mp4", "webm":
		return ArtifactKindVideo
	}
	return ArtifactKindFile
}

func artifactMimeFor(rec ArtifactRecord) string {
	if rec.Mime != "" {
		return rec.Mime
	}
	switch rec.Ext {
	case "png":
		return "image/png"
	case "jpg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	case "gif":
		return "image/gif"
	case "mp4":
		return "video/mp4"
	case "webm":
		return "video/webm"
	case "pdf":
		return "application/pdf"
	case "txt":
		return "text/plain; charset=utf-8"
	case "csv":
		return "text/csv"
	case "md":
		return "text/markdown; charset=utf-8"
	case "json":
		return "application/json"
	case "zip":
		return "application/zip"
	case "docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case "xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case "pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	}
	return "application/octet-stream"
}
