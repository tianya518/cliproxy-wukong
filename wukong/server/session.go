package server

import (
	"log"
	"sync"
	"time"

	sentinel "github.com/router-for-me/CLIProxyAPI/v7/wukong/sentinel"
)

// sessionEntry 单个会话条目
type sessionEntry struct {
	client   *sentinel.Client
	lastUsed time.Time
	token    string // 该 session 绑定的 ChatGPT token
	authID   string // 该 token 对应的凭证 ID（auth-dir 文件名），产物映射用；可能为空

	// returned 记录本会话已经返回给客户端的生图 file_id。续接轮次按整个会话的图槽重建列表，
	// 不做差集的话上一轮的旧图会在每一轮响应里重复出现。
	returnedMu sync.Mutex
	returned   map[string]struct{}
}

// newImageIDs 返回 ids 里尚未返回过的那部分（保持顺序）。
func (e *sessionEntry) newImageIDs(ids []string) []string {
	if e == nil || len(ids) == 0 {
		return ids
	}
	e.returnedMu.Lock()
	defer e.returnedMu.Unlock()
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, seen := e.returned[id]; seen {
			continue
		}
		out = append(out, id)
	}
	return out
}

// markReturned 记下已经返回过的生图 file_id。
func (e *sessionEntry) markReturned(ids []string) {
	if e == nil || len(ids) == 0 {
		return
	}
	e.returnedMu.Lock()
	defer e.returnedMu.Unlock()
	if e.returned == nil {
		e.returned = make(map[string]struct{}, len(ids))
	}
	for _, id := range ids {
		if id != "" {
			e.returned[id] = struct{}{}
		}
	}
}

// SessionManager 有状态多轮对话管理器
// key = conversationID（来自 ChatGPT 服务端，首轮对话后写入）
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*sessionEntry
	ttl      time.Duration
	cfg      *ServerConfig
}

// NewSessionManager 创建 Session 管理器
func NewSessionManager(cfg *ServerConfig) *SessionManager {
	sm := &SessionManager{
		sessions: make(map[string]*sessionEntry),
		ttl:      time.Duration(cfg.SessionTTLMinutes) * time.Minute,
		cfg:      cfg,
	}
	go sm.cleanupLoop()
	return sm
}

// GetSession 获取指定的 session
func (sm *SessionManager) GetSession(convID string) (*sessionEntry, bool) {
	if convID == "" {
		return nil, false
	}
	sm.mu.RLock()
	entry, ok := sm.sessions[convID]
	sm.mu.RUnlock()
	return entry, ok
}

// GetOrCreate 根据 conversationID 获取已有 session 或创建新 session
//   - convID == ""：创建新 Client（新对话），返回 entry
//   - convID != ""：查找已有 session，若不存在则新建（防止 session 过期后重建）
func (sm *SessionManager) GetOrCreate(convID, token string) *sessionEntry {
	if convID != "" {
		sm.mu.RLock()
		entry, ok := sm.sessions[convID]
		sm.mu.RUnlock()
		if ok {
			sm.mu.Lock()
			entry.lastUsed = time.Now()
			sm.mu.Unlock()
			return entry
		}
	}

	// 新建 Client
	client := sentinel.NewClient(sentinel.Config{
		BearerToken: token,
		Model:       sm.cfg.DefaultModel,
		TempMode:    sm.cfg.TempMode,
		ImageDir:    sm.cfg.ImageDir,
		ProxyURL:    sm.cfg.ProxyURL,
	})
	// 启用自动图片下载阻塞，确保 Web UI 能够获取并渲染图片
	client.SetDisableAutoImage(false)

	entry := &sessionEntry{
		client:   client,
		lastUsed: time.Now(),
		token:    token,
	}
	// 注意：此时 convID 可能为空，新对话的 conversationID 要等第一轮结束后才知道
	// 见 handler_chat.go 中在对话完成后调用 sm.Register()
	return entry
}

// Register 对话完成后，将 entry 注册到 conversationID 下
func (sm *SessionManager) Register(convID string, entry *sessionEntry) {
	if convID == "" {
		return
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	entry.lastUsed = time.Now()
	sm.sessions[convID] = entry
}

// Delete 主动删除一个 session
func (sm *SessionManager) Delete(convID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.sessions, convID)
}

// Count 返回当前活跃 session 数
func (sm *SessionManager) Count() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.sessions)
}

// cleanupLoop 后台定期清理过期 session
func (sm *SessionManager) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		sm.cleanup()
	}
}

func (sm *SessionManager) cleanup() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	now := time.Now()
	removed := 0
	for convID, entry := range sm.sessions {
		if now.Sub(entry.lastUsed) > sm.ttl {
			delete(sm.sessions, convID)
			removed++
		}
	}
	if removed > 0 {
		log.Printf("[session] 清理过期 session %d 个，当前活跃 %d 个", removed, len(sm.sessions))
	}
}
