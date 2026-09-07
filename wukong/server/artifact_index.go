package server

// artifact_index.go —— 产物映射表：每个产物"属于哪个 provider、哪个凭证、在上游怎么定位"。
//
// 这是产物存储的底：磁盘缓存可以被清理，映射表不会。缓存缺失时按映射回源补回。
// 落盘格式是追加写的 JSONL，一行一条；同 ID 多次出现以最后一条为准。启动时全量读入
// 内存，发现重复条目较多时压缩重写一次。每条几百字节，不设上限。

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ArtifactLocator 上游定位符，三种来源各填自己的字段。
type ArtifactLocator struct {
	// chatgpt-web：图片与沙箱文件都需要会话 ID。
	ConvID string `json:"conv_id,omitempty"`
	// chatgpt-web 图片：/backend-api/files/download/{file_id}
	FileID string `json:"file_id,omitempty"`
	// chatgpt-web 沙箱文件：/interpreter/download?message_id=&sandbox_path=
	MessageID   string `json:"message_id,omitempty"`
	SandboxPath string `json:"sandbox_path,omitempty"`
	// grok-web：assets.grok.com / imagine-public.x.ai 原始 URL
	URL string `json:"url,omitempty"`
}

// ArtifactRecord 一条映射：不透明 ID ↔ 来源与定位。
type ArtifactRecord struct {
	ID        string          `json:"id"`
	Ext       string          `json:"ext"`
	Mime      string          `json:"mime,omitempty"`
	Name      string          `json:"name,omitempty"` // 展示名（沙箱文件原名），可空
	Kind      string          `json:"kind,omitempty"` // image | video | file
	Provider  string          `json:"provider"`       // chatgpt-web / grok-web
	AuthID    string          `json:"auth_id,omitempty"`
	Locator   ArtifactLocator `json:"locator"`
	CreatedAt time.Time       `json:"created_at"`
}

// 产物类型。
const (
	ArtifactKindImage = "image"
	ArtifactKindVideo = "video"
	ArtifactKindFile  = "file"
)

var artifactIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// artifactExtAllowed 对外文件名允许的扩展名。
var artifactExtAllowed = map[string]bool{
	"png": true, "jpg": true, "jpeg": true, "webp": true, "gif": true,
	"mp4": true, "webm": true,
	"pdf": true, "txt": true, "csv": true, "md": true, "json": true,
	"docx": true, "xlsx": true, "pptx": true, "zip": true,
	"bin": true, // 类型未知时的兜底
}

// ValidArtifactID 报告 id 是否只含 [A-Za-z0-9_-] 且长度合理。
func ValidArtifactID(id string) bool { return artifactIDRe.MatchString(id) }

// NormalizeArtifactExt 规整扩展名：去点、小写、jpeg→jpg；不在白名单内返回空。
func NormalizeArtifactExt(ext string) string {
	ext = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
	if ext == "jpeg" {
		ext = "jpg"
	}
	if artifactExtAllowed[ext] {
		return ext
	}
	return ""
}

// artifactIndex 内存 map + JSONL 追加文件。
type artifactIndex struct {
	mu      sync.RWMutex
	path    string
	entries map[string]ArtifactRecord
	file    *os.File
	lines   int // 文件里的行数（含重复），用于判断是否需要压缩
}

func openArtifactIndex(path string) (*artifactIndex, error) {
	idx := &artifactIndex{path: path, entries: make(map[string]ArtifactRecord)}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("artifact index dir: %w", err)
	}
	if err := idx.load(); err != nil {
		return nil, err
	}
	// 重复条目过多时压缩一次，避免文件无限增长。
	if idx.lines > len(idx.entries)*2+256 {
		if err := idx.compact(); err != nil {
			log.Printf("[artifact] 索引压缩失败（继续使用原文件）: %v", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("artifact index open: %w", err)
	}
	idx.file = f
	return idx, nil
}

func (x *artifactIndex) load() error {
	f, err := os.Open(x.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("artifact index read: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	bad := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		x.lines++
		var rec ArtifactRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil || !ValidArtifactID(rec.ID) {
			bad++
			continue
		}
		x.entries[rec.ID] = rec
	}
	if bad > 0 {
		log.Printf("[artifact] 索引 %s 有 %d 行无法解析，已跳过", x.path, bad)
	}
	return sc.Err()
}

// compact 用内存里的去重结果重写文件（写临时文件再 rename）。
func (x *artifactIndex) compact() error {
	tmp := x.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, rec := range x.entries {
		b, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		w.Write(b)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, x.path); err != nil {
		return err
	}
	x.lines = len(x.entries)
	return nil
}

// put 登记或更新一条映射。已有条目只允许补充 AuthID / Name / Mime / Ext，定位符以先到为准
// （同一个产物不可能换来源）；没有任何变化时不写盘。
func (x *artifactIndex) put(rec ArtifactRecord) error {
	if !ValidArtifactID(rec.ID) {
		return fmt.Errorf("artifact index: invalid id %q", rec.ID)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if old, ok := x.entries[rec.ID]; ok {
		merged := old
		changed := false
		if rec.AuthID != "" && rec.AuthID != old.AuthID {
			merged.AuthID = rec.AuthID
			changed = true
		}
		if rec.Name != "" && rec.Name != old.Name {
			merged.Name = rec.Name
			changed = true
		}
		if rec.Mime != "" && rec.Mime != old.Mime {
			merged.Mime = rec.Mime
			changed = true
		}
		if rec.Ext != "" && rec.Ext != old.Ext {
			merged.Ext = rec.Ext
			changed = true
		}
		// 定位符缺失的字段允许补齐（例如流中会话 ID 尚未知时先登记、事后补上）。
		if old.Locator.ConvID == "" && rec.Locator.ConvID != "" {
			merged.Locator.ConvID = rec.Locator.ConvID
			changed = true
		}
		if !changed {
			return nil
		}
		rec = merged
	} else if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	x.entries[rec.ID] = rec
	return x.appendLocked(rec)
}

func (x *artifactIndex) appendLocked(rec ArtifactRecord) error {
	if x.file == nil {
		return nil
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := x.file.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("artifact index append: %w", err)
	}
	x.lines++
	return nil
}

func (x *artifactIndex) get(id string) (ArtifactRecord, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	rec, ok := x.entries[id]
	return rec, ok
}

func (x *artifactIndex) count() int {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return len(x.entries)
}

func (x *artifactIndex) close() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.file == nil {
		return nil
	}
	err := x.file.Close()
	x.file = nil
	return err
}
