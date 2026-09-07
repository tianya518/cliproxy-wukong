package server

// engine.go —— 与传输层解耦的对话内核。
//
// 这里承载 /v1/chat/completions 的全部编排逻辑，但不依赖 gin：
// HTTP 服务（handler_chat.go）和 cliproxy executor（cliproxy/ 子模块）
// 都通过 Engine 驱动同一份实现，避免两处各写一遍 OpenAI ↔ sentinel 的转换。
//
// gin 相关的输入（token、请求上下文、绝对 URL 拼接）统一收敛到 ChatEnv，
// 输出则通过返回值或 emit 回调交还调用方自行落到具体协议上。

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	sentinel "github.com/router-for-me/CLIProxyAPI/v7/wukong/sentinel"
)

// ErrNoInput 请求里既无文本也无图片，无法构成一轮对话。
var ErrNoInput = errors.New("no user message or images found in messages")

// ChatEnv 单次请求的环境依赖，由调用方按自身传输层填充。
type ChatEnv struct {
	// Ctx 控制上传附件等子请求的生命周期。
	Ctx context.Context
	// Token 本轮使用的 ChatGPT 凭证。
	Token string
	// FromPool 标记凭证来自内置池——只有这种情况下鉴权失败才值得换票重试。
	FromPool bool
	// AuthID 本轮凭证在 cliproxy 凭证池里的 ID（auth-dir 文件名）。产物映射记录它，
	// 缓存缺失时据此找回当前有效凭证回源。可为空（无池形态）。
	AuthID string
	// AbsoluteURL 把 /api/image/proxy 这类相对路径拼成末端客户端可达的绝对地址。
	// 为空时产物链接保持相对路径。
	AbsoluteURL func(path string) string
}

func (e ChatEnv) ctx() context.Context {
	if e.Ctx == nil {
		return context.Background()
	}
	return e.Ctx
}

func (e ChatEnv) absolute(path string) string {
	if e.AbsoluteURL == nil {
		return path
	}
	return e.AbsoluteURL(path)
}

// Engine 持有对话所需的长生命周期依赖。
type Engine struct {
	cfg     *ServerConfig
	pool    *TokenPool
	session *SessionManager
	store   *ArtifactStore // 可为空：没有产物存储时链接退回 /api/*/proxy 会话代理
}

// NewEngine 创建对话内核。
func NewEngine(cfg *ServerConfig, pool *TokenPool, session *SessionManager) *Engine {
	return &Engine{cfg: cfg, pool: pool, session: session}
}

// SetArtifactStore 挂上产物存储：生图 / 沙箱文件先登记映射再落缓存，链接改为 /files/<id>。
func (e *Engine) SetArtifactStore(store *ArtifactStore) {
	if e != nil {
		e.store = store
	}
}

// preparedTurn 一轮对话在真正发起前解析好的全部参数。
type preparedTurn struct {
	entry     *sessionEntry
	opts      sentinel.ChatOptions
	apiModel  string
	chatID    string
	createdAt int64
}

// prepare 解析请求、取 session、上传附件、解析模型，组装 ChatOptions。
func (e *Engine) prepare(env ChatEnv, req *ChatCompletionRequest) (*preparedTurn, error) {
	if req.Model == "" {
		req.Model = e.cfg.DefaultModel
	}

	userMsg, systemPrompt, b64Images := extractUserMessage(req.Messages)
	if userMsg == "" && len(b64Images) == 0 {
		return nil, ErrNoInput
	}

	entry := e.session.GetOrCreate(req.ConversationID, env.Token)
	if env.AuthID != "" {
		entry.authID = env.AuthID
	}
	if req.ConversationID != "" {
		e.session.Register(req.ConversationID, entry)
	}

	// 无 conversationID 时上游会话不持有上下文，需把历史轮次与 system prompt 一并展平进本轮输入。
	// 历史里本网关生成过的图（/files/<id> 链接或客户端回传的 images[]）会重新挂进本轮，
	// 否则模型只看得到一行它打不开的 URL 文本。
	inputMsg := userMsg
	var historyRefs []string
	if req.ConversationID == "" {
		history := flattenHistory(req.Messages)
		var note string
		history, historyRefs, note = e.reattachHistoryImages(req.Messages, history)
		if history != "" {
			inputMsg = "[Conversation so far]\n" + history + "\n\n"
			if note != "" {
				inputMsg += note + "\n\n"
			}
			inputMsg += "[Current message]\n" + userMsg
		}
		if systemPrompt != "" && entry.client.GetModel() != "" {
			inputMsg = "[System]: " + systemPrompt + "\n\n" + inputMsg
		}
	}

	uploadedImages := e.uploadAttachments(env, entry, append(historyRefs, b64Images...))

	resolved := sentinel.ResolveChatModel(req.Model)
	apiModel := resolved.APIModel
	if apiModel == "" {
		apiModel = req.Model
	}

	// 切换模型（生图别名会映射为 dall-e-3）
	if resolved.ChatModel != "" && resolved.ChatModel != entry.client.GetModel() {
		entry.client.SetModel(resolved.ChatModel)
	}

	// 生图必须走正式会话：临时会话里上游不挂载 image_gen 工具，会直接以文本拒绝出图。
	// 其余请求沿用配置的临时模式，避免账号级跨会话记忆把无关请求的内容带进来。
	imageRequest := resolved.ForcePictureV2 || req.PictureV2
	entry.client.SetTempMode(e.cfg.TempMode && !imageRequest)

	if gid := req.resolvedGizmoID(); gid != "" {
		entry.client.SetGizmoID(gid)
	}
	// 项目对话必须落进官网项目，临时模式不会挂 gizmo。
	if entry.client.GizmoID() != "" {
		entry.client.SetTempMode(false)
	}

	return &preparedTurn{
		entry: entry,
		opts: sentinel.ChatOptions{
			Text:           inputMsg,
			Images:         uploadedImages,
			ForcePictureV2: imageRequest,
			ImageAspect:    sizeToAspect(req.Size),
			// ThinkingEffort 由模型解析表确定（空串 = 不携带字段，对应极速/o3 等）
			ThinkingEffort: resolved.ThinkingEffort,
			GizmoID:        entry.client.GizmoID(),
		},
		apiModel:  apiModel,
		chatID:    "chatcmpl-" + sentinel.GenerateUUID(),
		createdAt: time.Now().Unix(),
	}, nil
}

// uploadAttachments 把 data URL / HTTP URL 形式的附件下载并上传到上游。
func (e *Engine) uploadAttachments(env ChatEnv, entry *sessionEntry, refs []string) []sentinel.UploadedFile {
	var out []sentinel.UploadedFile
	for _, b64 := range refs {
		var data []byte
		var fileName, mimeHint string
		var err error

		if strings.HasPrefix(b64, artifactRefScheme) {
			// 本网关产物存储里的图（历史回挂）：直接读缓存 / 回源，不走 HTTP。
			if e.store == nil {
				continue
			}
			id := strings.TrimPrefix(b64, artifactRefScheme)
			var rec ArtifactRecord
			data, rec, err = e.store.ReadBytes(env.ctx(), id, 0)
			if err != nil || len(data) == 0 {
				fmt.Printf("[artifact] 历史图片 %s 读取失败，跳过回挂: %v\n", id, err)
				continue
			}
			mimeHint = artifactMimeFor(rec)
			fileName = rec.Name
			if fileName == "" {
				fileName = rec.ID + "." + rec.Ext
			}
		} else if e.store != nil && func() bool { _, ok := e.store.IDFromPublicURL(b64); return ok }() {
			// 客户端把本网关的 /files 链接当 image_url 传回来：同样直接读存储，不绕一圈 HTTP。
			id, _ := e.store.IDFromPublicURL(b64)
			var rec ArtifactRecord
			data, rec, err = e.store.ReadBytes(env.ctx(), id, 0)
			if err != nil || len(data) == 0 {
				continue
			}
			mimeHint = artifactMimeFor(rec)
			fileName = rec.Name
			if fileName == "" {
				fileName = rec.ID + "." + rec.Ext
			}
		} else if strings.HasPrefix(b64, "http://") || strings.HasPrefix(b64, "https://") {
			// HTTP/HTTPS URL：先下载再上传
			data, fileName, mimeHint, err = downloadURL(b64)
			if err != nil || len(data) == 0 {
				continue
			}
		} else if strings.HasPrefix(b64, "data:") {
			// 解析 data URL：data:<mime>;base64,<data>  或  data:<mime>,<data>
			commaIdx := strings.Index(b64, ",")
			if commaIdx < 0 {
				continue
			}
			header := b64[5:commaIdx]   // e.g. "application/pdf;base64" or "image/jpeg;base64"
			payload := b64[commaIdx+1:] // base64 encoded data

			if strings.Contains(header, ";base64") {
				data, err = base64.StdEncoding.DecodeString(payload)
			} else {
				data = []byte(payload)
			}
			if err != nil || len(data) == 0 {
				continue
			}
			mimeHint = strings.TrimSuffix(header, ";base64")
			fileName = guessFileName(mimeHint)
		} else {
			continue
		}

		uf, uploadErr := entry.client.UploadFile(env.ctx(), data, fileName, mimeHint)
		if uploadErr == nil && uf != nil {
			out = append(out, *uf)
		}
	}
	return out
}

// imageArtifactURL 生图链接。挂了产物存储时先登记映射再给 /files/<id>.png；否则退回会话代理链接。
func (e *Engine) imageArtifactURL(env ChatEnv, authID, convID, fileID string) string {
	if e.store != nil && strings.TrimSpace(fileID) != "" {
		rec := ArtifactRecord{
			ID: ChatGPTImageArtifactID(fileID), Ext: "png", Mime: "image/png", Kind: ArtifactKindImage,
			Provider: chatGPTArtifactProvider, AuthID: authID,
			Locator: ArtifactLocator{ConvID: convID, FileID: fileID},
		}
		if err := e.store.Record(rec); err == nil {
			if stored, ok := e.store.Lookup(rec.ID); ok {
				rec = stored
			}
			return e.store.PublicURL(env.absolute, rec)
		} else {
			fmt.Printf("[artifact] 登记生图映射失败，退回会话代理链接: %v\n", err)
		}
	}
	return env.absolute(fmt.Sprintf("/api/image/proxy?conv_id=%s&file_id=%s", convID, fileID))
}

// sandboxArtifactURL 沙箱文件链接，规则同 imageArtifactURL。
func (e *Engine) sandboxArtifactURL(env ChatEnv, authID, convID, messageID, sandboxPath string) string {
	if e.store != nil && strings.TrimSpace(sandboxPath) != "" {
		name := path.Base(sandboxPath)
		ext := ArtifactExtForName(name)
		if ext == "" {
			ext = "bin"
		}
		rec := ArtifactRecord{
			ID: SandboxArtifactID(convID, messageID, sandboxPath), Ext: ext, Name: name, Kind: ArtifactKindFile,
			Provider: chatGPTArtifactProvider, AuthID: authID,
			Locator: ArtifactLocator{ConvID: convID, MessageID: messageID, SandboxPath: sandboxPath},
		}
		if err := e.store.Record(rec); err == nil {
			if stored, ok := e.store.Lookup(rec.ID); ok {
				rec = stored
			}
			return e.store.PublicURL(env.absolute, rec)
		} else {
			fmt.Printf("[artifact] 登记沙箱文件映射失败，退回会话代理链接: %v\n", err)
		}
	}
	return env.absolute(fmt.Sprintf("/api/pdf/proxy?conv_id=%s&msg_id=%s&sandbox_path=%s",
		convID, messageID, url.QueryEscape(sandboxPath)))
}

// buildArtifactConfig 组装产物流式配置（生图/沙箱文件的链接构造与事件回调）。
func (e *Engine) buildArtifactConfig(env ChatEnv, entry *sessionEntry, req ChatCompletionRequest, convID string, onEvent func(sentinel.StreamEvent)) sentinel.ArtifactStreamConfig {
	authID := env.AuthID
	if authID == "" && entry != nil {
		authID = entry.authID
	}
	return sentinel.ArtifactStreamConfig{
		Delivery:       req.ArtifactDelivery,
		ChunkSize:      req.ArtifactBase64ChunkSize,
		ImageRevisions: req.ArtifactImageRevisions,
		OnEvent:        onEvent,
		BuildImageURL: func(fileID string) string {
			cid := convID
			if cid == "" && entry != nil {
				cid = entry.client.GetSessionInfo().ConversationID
			}
			return e.imageArtifactURL(env, authID, cid, fileID)
		},
		BuildSandboxURL: func(messageID, sandboxPath string) string {
			return e.sandboxArtifactURL(env, authID, convID, messageID, sandboxPath)
		},
	}
}

// persistArtifacts 结果就位后把本轮产物写进产物存储：
//   - 先用最终的会话 ID 补齐映射（流中登记时会话 ID 可能还没拿到）；
//   - 图片同步下载写缓存（内联 base64 与后续访问都靠这份字节），最多 4 张并行；
//   - 沙箱文件后台下载，不阻塞响应。
//
// 任何一步失败都只记日志：映射已在，链接照发，访问时按映射回源。
func (e *Engine) persistArtifacts(env ChatEnv, entry *sessionEntry, result *sentinel.ChatResult, convID string) {
	if e.store == nil || result == nil || entry == nil || convID == "" {
		return
	}
	authID := env.AuthID
	if authID == "" {
		authID = entry.authID
	}

	fileIDs := result.ImageFileIDs
	if len(fileIDs) == 0 && result.ImageFileID != "" {
		fileIDs = []string{result.ImageFileID}
	}
	if result.ExpectGeneratedImages && len(fileIDs) > 0 {
		var wg sync.WaitGroup
		sem := make(chan struct{}, 4)
		for _, fileID := range fileIDs {
			e.imageArtifactURL(env, authID, convID, fileID) // 补齐 ConvID / AuthID
			id := ChatGPTImageArtifactID(fileID)
			if e.store.Cached(id) {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(fileID, id string) {
				defer wg.Done()
				defer func() { <-sem }()
				data, mimeType, err := entry.client.DownloadFileByFileID(convID, fileID)
				if err != nil || len(data) == 0 {
					fmt.Printf("[artifact] 生图 %s 预下载失败（访问时回源）: %v\n", fileID, err)
					return
				}
				if err := e.store.PutBytes(id, data, mimeType); err != nil {
					fmt.Printf("[artifact] 生图 %s 写缓存失败: %v\n", fileID, err)
				}
			}(fileID, id)
		}
		wg.Wait()
	}

	for _, f := range sandboxFilesForHandler(result) {
		msgID := f.MessageID
		if msgID == "" {
			msgID = result.LastAssistantMsgID
		}
		e.sandboxArtifactURL(env, authID, convID, msgID, f.SandboxPath)
		id := SandboxArtifactID(convID, msgID, f.SandboxPath)
		if e.store.Cached(id) {
			continue
		}
		go func(msgID, sandboxPath, id string) {
			data, mimeType, err := entry.client.DownloadSandboxFile(convID, msgID, sandboxPath)
			if err != nil || len(data) == 0 {
				fmt.Printf("[artifact] 沙箱文件 %s 预下载失败（访问时回源）: %v\n", sandboxPath, err)
				return
			}
			if err := e.store.PutBytes(id, data, mimeType); err != nil {
				fmt.Printf("[artifact] 沙箱文件 %s 写缓存失败: %v\n", sandboxPath, err)
			}
		}(msgID, f.SandboxPath, id)
	}
}

// resultImageFileIDs 本轮结果里的生图 file_id（多图列表优先，退回单图兼容字段）。
func resultImageFileIDs(result *sentinel.ChatResult) []string {
	if result == nil || !result.ExpectGeneratedImages {
		return nil
	}
	if len(result.ImageFileIDs) > 0 {
		return result.ImageFileIDs
	}
	if result.ImageFileID != "" {
		return []string{result.ImageFileID}
	}
	return nil
}

// imagesToReturn 决定这一轮要交给客户端的生图：新会话全给；带 conversation_id 续接时只给本轮
// 新增的（官网按整个会话的图槽重建列表，不做差集会让旧图每轮重复出现）。
func (e *Engine) imagesToReturn(entry *sessionEntry, req ChatCompletionRequest, result *sentinel.ChatResult) []string {
	ids := resultImageFileIDs(result)
	if req.ConversationID == "" || entry == nil {
		return ids
	}
	return entry.newImageIDs(ids)
}

// inlineImages 把本轮生图读成 images[]（data URL）。没挂存储或关闭内联时为 nil。
func (e *Engine) inlineImages(env ChatEnv, fileIDs []string) []ImagePart {
	if e.store == nil || len(fileIDs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(fileIDs))
	for _, fileID := range fileIDs {
		ids = append(ids, ChatGPTImageArtifactID(fileID))
	}
	return e.store.InlineImageParts(env.ctx(), ids)
}

// artifactMarkdown 把生图与沙箱产物整理成 markdown 链接，供不认 sentinel 扩展字段的标准客户端展示。
// imageFileIDs 由调用方决定（见 imagesToReturn）；沙箱文件与本地图片路径仍从 result 取。
func (e *Engine) artifactMarkdown(env ChatEnv, entry *sessionEntry, req ChatCompletionRequest, result *sentinel.ChatResult, convID string, imageFileIDs []string) string {
	if !req.wantArtifactMarkdown() {
		return ""
	}
	authID := env.AuthID
	if authID == "" && entry != nil {
		authID = entry.authID
	}
	var b strings.Builder

	if result.ExpectGeneratedImages {
		switch {
		case len(imageFileIDs) > 1:
			for i, fileID := range imageFileIDs {
				fmt.Fprintf(&b, "\n\n![Generated Image %d](%s)", i+1, e.imageArtifactURL(env, authID, convID, fileID))
			}
		case len(imageFileIDs) == 1:
			fmt.Fprintf(&b, "\n\n![Generated Image](%s)", e.imageArtifactURL(env, authID, convID, imageFileIDs[0]))
		case len(resultImageFileIDs(result)) > 0:
			// 本轮没有新图（续接轮次全是旧图）：不重复贴链接
		case result.ImagePath != "":
			p := result.ImagePath
			if !strings.HasPrefix(p, "http://") && !strings.HasPrefix(p, "https://") {
				p = strings.ReplaceAll(p, "\\", "/")
				if !strings.HasPrefix(p, "/") {
					p = "/" + p
				}
			}
			fmt.Fprintf(&b, "\n\n![Generated Image](%s)", env.absolute(p))
		}
	}

	for i, f := range sandboxFilesForHandler(result) {
		msgID := f.MessageID
		if msgID == "" {
			msgID = result.LastAssistantMsgID
		}
		label := f.FileName
		if label == "" {
			label = fmt.Sprintf("file_%d", i+1)
		}
		fmt.Fprintf(&b, "\n\n[%s](%s)", label, e.sandboxArtifactURL(env, authID, convID, msgID, f.SandboxPath))
	}
	return b.String()
}

// reasoningText 把思考步骤汇总成非流式响应里的 reasoning_content。
func reasoningText(result *sentinel.ChatResult) string {
	if len(result.ThinkSteps) == 0 {
		return result.ThinkingText
	}
	var sb strings.Builder
	for i, step := range result.ThinkSteps {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		fmt.Fprintf(&sb, "**%s**\n%s", step.Summary, step.Content)
	}
	return sb.String()
}

// ─── 非流式 ──────────────────────────────────────────────────────────────────

// Complete 执行一轮非流式对话。
func (e *Engine) Complete(env ChatEnv, req ChatCompletionRequest) (*ChatCompletionResponse, error) {
	turn, err := e.prepare(env, &req)
	if err != nil {
		return nil, err
	}

	var sentinelEvents []sentinel.StreamEvent
	collect := func(ev sentinel.StreamEvent) { sentinelEvents = append(sentinelEvents, ev) }

	convForArt := req.ConversationID
	registerSessionForConv := func(convID string) {
		if convID == "" {
			return
		}
		convForArt = convID
		e.session.Register(convID, turn.entry)
		turn.opts.Artifacts = e.buildArtifactConfig(env, turn.entry, req, convID, collect)
	}
	turn.opts.OnConversationID = registerSessionForConv
	if req.ConversationID != "" {
		registerSessionForConv(req.ConversationID)
	}
	turn.opts.Artifacts = e.buildArtifactConfig(env, turn.entry, req, convForArt, collect)

	result, err := e.chatWithRetry(env, turn.entry, turn.opts)
	if err != nil {
		return nil, err
	}

	if result.ConversationID != "" {
		registerSessionForConv(result.ConversationID)
	}

	if result.ExpectGeneratedImages {
		turn.entry.client.FinishImageGenWS(result, turn.opts)
	}
	turn.entry.client.EmitNewArtifacts(turn.opts.Artifacts, result)
	e.persistArtifacts(env, turn.entry, result, result.ConversationID)
	imageIDs := e.imagesToReturn(turn.entry, req, result)

	content := result.Text
	sentinel.LogContentPreview(func(format string, args ...interface{}) {
		fmt.Printf("[chat-response] "+format+"\n", args...)
	}, "client-body", content)

	content += e.artifactMarkdown(env, turn.entry, req, result, result.ConversationID, imageIDs)
	inline := e.inlineImages(env, imageIDs)
	turn.entry.markReturned(resultImageFileIDs(result))

	return &ChatCompletionResponse{
		ID:      turn.chatID,
		Object:  "chat.completion",
		Created: turn.createdAt,
		Model:   turn.apiModel,
		Choices: []Choice{{
			Index:            0,
			Message:          Message{Role: "assistant", Content: content, Images: inline},
			FinishReason:     "stop",
			ReasoningContent: reasoningText(result),
		}},
		Usage:          Usage{},
		ConversationID: result.ConversationID,
		Sentinel:       sentinelEvents,
	}, nil
}

// ─── 流式 ────────────────────────────────────────────────────────────────────

// Stream 执行一轮流式对话，每个增量通过 emit 交给调用方。
// emit 只负责把 chunk 落到具体传输层，终止标记（如 SSE 的 [DONE]）由调用方补。
func (e *Engine) Stream(env ChatEnv, req ChatCompletionRequest, emit func(ChatCompletionChunk)) error {
	turn, err := e.prepare(env, &req)
	if err != nil {
		return err
	}

	includeThinking := req.IncludeThinking || req.PictureV2
	chunk := func(delta Delta, finish *string) ChatCompletionChunk {
		return ChatCompletionChunk{
			ID: turn.chatID, Object: "chat.completion.chunk", Created: turn.createdAt, Model: turn.apiModel,
			Choices: []ChunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
		}
	}

	firstSent := false
	streamedToClient := strings.Builder{}
	registeredConvID := req.ConversationID

	writeSentinel := func(ev sentinel.StreamEvent) {
		c := chunk(Delta{}, nil)
		c.Sentinel = &ev
		emit(c)
	}

	registerSessionForConv := func(convID string) {
		if convID == "" {
			return
		}
		registeredConvID = convID
		e.session.Register(convID, turn.entry)
		turn.opts.Artifacts = e.buildArtifactConfig(env, turn.entry, req, convID, writeSentinel)
	}
	turn.opts.OnConversationID = registerSessionForConv
	registerSessionForConv(req.ConversationID)
	turn.opts.Artifacts = e.buildArtifactConfig(env, turn.entry, req, registeredConvID, writeSentinel)

	handler := func(delta string) {
		if !includeThinking && len(delta) > 0 && delta[0] == '\x00' {
			return
		}
		if !firstSent {
			// 第一个有内容的 chunk，先发 role
			emit(chunk(Delta{Role: "assistant"}, nil))
			firstSent = true
		}
		streamedToClient.WriteString(delta)
		emit(chunk(Delta{Content: delta}, nil))
	}

	result, err := e.chatStreamWithRetry(env, turn.entry, turn.opts, sentinel.StreamHandler(handler))
	if err != nil {
		tokenPreview := turn.entry.token
		if len(tokenPreview) > 20 {
			tokenPreview = tokenPreview[:10] + "..." + tokenPreview[len(tokenPreview)-8:]
		}
		fmt.Printf("[chat-err] token=%s error=%v\n", tokenPreview, err)
		return err
	}

	if result.ConversationID != "" {
		registerSessionForConv(result.ConversationID)
	}

	sentinel.LogContentPreview(func(format string, args ...interface{}) {
		fmt.Printf("[chat-stream-client] "+format+"\n", args...)
	}, "stream-deltas", streamedToClient.String())
	sentinel.LogContentPreview(func(format string, args ...interface{}) {
		fmt.Printf("[chat-stream-upstream] "+format+"\n", args...)
	}, "result-text", result.Text)

	// 流式增量未发出/未发全时，用 result.Text 补齐（WS 中断后 conversation 恢复常见）
	streamed := streamedToClient.String()
	if result.Text != "" {
		var missing string
		switch {
		case streamed == "":
			missing = result.Text
		case strings.HasPrefix(result.Text, streamed) && len(result.Text) > len(streamed):
			missing = result.Text[len(streamed):]
		}
		if missing != "" {
			if !firstSent {
				emit(chunk(Delta{Role: "assistant"}, nil))
				firstSent = true
			}
			emit(chunk(Delta{Content: missing}, nil))
			streamedToClient.WriteString(missing)
		}
	}

	// 思考步骤详细内容（流结束后推送，仅 Web UI 请求 include_thinking 时）
	if includeThinking && len(result.ThinkSteps) > 0 {
		var thinkContent strings.Builder
		thinkContent.WriteString("\x00THINK_DETAILS\x00")
		for i, step := range result.ThinkSteps {
			if i > 0 {
				thinkContent.WriteString("\x00STEP_SEP\x00")
			}
			thinkContent.WriteString(step.Summary)
			thinkContent.WriteString("\x1F")
			thinkContent.WriteString(step.Content)
		}
		emit(chunk(Delta{Content: thinkContent.String()}, nil))
	}

	if result.ExpectGeneratedImages {
		turn.entry.client.FinishImageGenWS(result, turn.opts)
	}
	// 兜底：沙箱等未在流中推送的产物
	turn.entry.client.EmitNewArtifacts(turn.opts.Artifacts, result)
	e.persistArtifacts(env, turn.entry, result, registeredConvID)
	imageIDs := e.imagesToReturn(turn.entry, req, result)

	fmt.Printf("[chat-done] model=%s conv=%s expect_img=%v image_ids=%v new=%d %s text_len=%d streamed=%d\n",
		turn.apiModel, result.ConversationID, result.ExpectGeneratedImages, result.ImageFileIDs, len(imageIDs),
		result.ImageGenDiagSummary(), len(result.Text), streamedToClient.Len())

	// 兼容：可选 markdown 链接（旧客户端）
	if md := e.artifactMarkdown(env, turn.entry, req, result, registeredConvID, imageIDs); md != "" {
		emit(chunk(Delta{Content: md}, nil))
	}

	// 内联 base64 与 finish_reason 同一个 chunk 给出（与 cliproxy Codex 通道一致）。
	inline := e.inlineImages(env, imageIDs)
	turn.entry.markReturned(resultImageFileIDs(result))

	stopReason := "stop"
	stop := chunk(Delta{Images: inline}, &stopReason)
	stop.ConversationID = registeredConvID
	emit(stop)
	return nil
}
