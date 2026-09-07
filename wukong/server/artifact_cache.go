package server

// artifact_cache.go —— 产物字节的磁盘缓存。
//
// 文件名 <id>.<ext>，先写 .tmp 再 rename，读到的永远是完整文件。受总大小 / 天数上限
// 约束做最旧优先清理；清理只删字节，映射表（artifact_index.go）不动，被删的产物下次
// 访问按映射回源补回。

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type artifactCache struct {
	dir           string
	maxTotalBytes int64
	maxAge        time.Duration

	mu       sync.RWMutex
	present  map[string]string // id → 文件名
	lastSeen map[string]time.Time
}

func openArtifactCache(dir string, maxTotalBytes int64, maxAge time.Duration) (*artifactCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("artifact cache dir: %w", err)
	}
	c := &artifactCache{
		dir:           dir,
		maxTotalBytes: maxTotalBytes,
		maxAge:        maxAge,
		present:       make(map[string]string),
		lastSeen:      make(map[string]time.Time),
	}
	c.rescan()
	return c, nil
}

// rescan 从目录重建"已缓存"集合（启动时调用一次）。
func (c *artifactCache) rescan() {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".json") {
			continue
		}
		id, ext := splitArtifactName(name)
		if id == "" || ext == "" {
			continue
		}
		c.present[id] = name
	}
}

// splitArtifactName 把 "<id>.<ext>" 拆开；不合法返回空。
func splitArtifactName(name string) (id, ext string) {
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 || dot == len(name)-1 {
		return "", ""
	}
	id, ext = name[:dot], NormalizeArtifactExt(name[dot+1:])
	if !ValidArtifactID(id) || ext == "" {
		return "", ""
	}
	return id, ext
}

func (c *artifactCache) fileName(id, ext string) string { return id + "." + ext }

// put 把 r 的内容写成 <id>.<ext>，返回写入字节数。
func (c *artifactCache) put(id, ext string, r io.Reader) (int64, error) {
	if !ValidArtifactID(id) {
		return 0, fmt.Errorf("artifact cache: invalid id %q", id)
	}
	ext = NormalizeArtifactExt(ext)
	if ext == "" {
		return 0, fmt.Errorf("artifact cache: unsupported ext for %q", id)
	}
	final := filepath.Join(c.dir, c.fileName(id, ext))
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		if copyErr != nil {
			return 0, copyErr
		}
		return 0, closeErr
	}
	if n == 0 {
		_ = os.Remove(tmp)
		return 0, errors.New("artifact cache: empty body")
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	c.mu.Lock()
	// 同一 id 换了扩展名时删掉旧文件，避免残留占空间。
	if old, ok := c.present[id]; ok && old != c.fileName(id, ext) {
		_ = os.Remove(filepath.Join(c.dir, old))
	}
	c.present[id] = c.fileName(id, ext)
	c.lastSeen[id] = time.Now()
	c.mu.Unlock()
	return n, nil
}

// has 报告 id 是否已缓存，并返回实际文件名。
func (c *artifactCache) has(id string) (string, bool) {
	c.mu.RLock()
	name, ok := c.present[id]
	c.mu.RUnlock()
	if !ok {
		return "", false
	}
	if _, err := os.Stat(filepath.Join(c.dir, name)); err != nil {
		c.mu.Lock()
		delete(c.present, id)
		c.mu.Unlock()
		return "", false
	}
	return name, true
}

// open 打开已缓存文件；返回 *os.File 以便 http.ServeContent 做 Range。
func (c *artifactCache) open(id string) (*os.File, os.FileInfo, error) {
	name, ok := c.has(id)
	if !ok {
		return nil, nil, os.ErrNotExist
	}
	f, err := os.Open(filepath.Join(c.dir, name))
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	c.touch(id)
	return f, info, nil
}

// touch 记录访问时间，供清理时做 LRU；每小时最多落一次 mtime，避免热文件反复写元数据。
func (c *artifactCache) touch(id string) {
	now := time.Now()
	c.mu.Lock()
	last := c.lastSeen[id]
	c.lastSeen[id] = now
	name := c.present[id]
	c.mu.Unlock()
	if name != "" && now.Sub(last) > time.Hour {
		_ = os.Chtimes(filepath.Join(c.dir, name), now, now)
	}
}

func (c *artifactCache) remove(id string) {
	c.mu.Lock()
	name := c.present[id]
	delete(c.present, id)
	delete(c.lastSeen, id)
	c.mu.Unlock()
	if name != "" {
		_ = os.Remove(filepath.Join(c.dir, name))
	}
}

type cacheEntry struct {
	id    string
	name  string
	size  int64
	mtime time.Time
}

func (c *artifactCache) list() []cacheEntry {
	c.mu.RLock()
	ids := make([]string, 0, len(c.present))
	for id := range c.present {
		ids = append(ids, id)
	}
	c.mu.RUnlock()
	out := make([]cacheEntry, 0, len(ids))
	for _, id := range ids {
		c.mu.RLock()
		name := c.present[id]
		seen := c.lastSeen[id]
		c.mu.RUnlock()
		info, err := os.Stat(filepath.Join(c.dir, name))
		if err != nil {
			continue
		}
		mtime := info.ModTime()
		if seen.After(mtime) {
			mtime = seen
		}
		out = append(out, cacheEntry{id: id, name: name, size: info.Size(), mtime: mtime})
	}
	return out
}

// cleanup 按天数与总大小做一次清理，返回删除数量与释放字节。
func (c *artifactCache) cleanup(now time.Time) (removed int, freed int64) {
	entries := c.list()
	sort.Slice(entries, func(i, j int) bool { return entries[i].mtime.Before(entries[j].mtime) })

	var total int64
	for _, e := range entries {
		total += e.size
	}
	keep := make([]cacheEntry, 0, len(entries))
	for _, e := range entries {
		if c.maxAge > 0 && now.Sub(e.mtime) > c.maxAge {
			c.remove(e.id)
			removed++
			freed += e.size
			total -= e.size
			continue
		}
		keep = append(keep, e)
	}
	if c.maxTotalBytes > 0 {
		for _, e := range keep {
			if total <= c.maxTotalBytes {
				break
			}
			c.remove(e.id)
			removed++
			freed += e.size
			total -= e.size
		}
	}
	return removed, freed
}

// runCleanup 后台定期清理，ctx 结束即退出。
func (c *artifactCache) runCleanup(done <-chan struct{}, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if removed, freed := c.cleanup(time.Now()); removed > 0 {
				log.Printf("[artifact] 缓存清理：删除 %d 个文件，释放 %.1f MB", removed, float64(freed)/(1<<20))
			}
		}
	}
}
