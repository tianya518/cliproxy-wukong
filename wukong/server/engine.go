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
	"strings"
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
}

// NewEngine 创建对话内核。
func NewEngine(cfg *ServerConfig, pool *TokenPool, session *SessionManager) *Engine {
	return &Engine{cfg: cfg, pool: pool, session: session}
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
	if req.ConversationID != "" {
		e.session.Register(req.ConversationID, entry)
	}

	// 无 conversationID 时上游会话不持有上下文，需把历史轮次与 system prompt 一并展平进本轮输入
	inputMsg := userMsg
	if req.ConversationID == "" {
		if history := flattenHistory(req.Messages); history != "" {
			inputMsg = "[Conversation so far]\n" + history + "\n\n[Current message]\n" + userMsg
		}
		if systemPrompt != "" && entry.client.GetModel() != "" {
			inputMsg = "[System]: " + systemPrompt + "\n\n" + inputMsg
		}
	}

	uploadedImages := e.uploadAttachments(env, entry, b64Images)

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

		if strings.HasPrefix(b64, "http://") || strings.HasPrefix(b64, "https://") {
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

// buildArtifactConfig 组装产物流式配置（生图/沙箱文件的代理 URL 构造与事件回调）。
func (e *Engine) buildArtifactConfig(env ChatEnv, entry *sessionEntry, req ChatCompletionRequest, convID string, onEvent func(sentinel.StreamEvent)) sentinel.ArtifactStreamConfig {
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
			return env.absolute(fmt.Sprintf("/api/image/proxy?conv_id=%s&file_id=%s", cid, fileID))
		},
		BuildSandboxURL: func(messageID, sandboxPath string) string {
			return env.absolute(fmt.Sprintf("/api/pdf/proxy?conv_id=%s&msg_id=%s&sandbox_path=%s",
				convID, messageID, url.QueryEscape(sandboxPath)))
		},
	}
}

// artifactMarkdown 把生图与沙箱产物整理成 markdown 链接，供不认 sentinel 扩展字段的标准客户端展示。
func (e *Engine) artifactMarkdown(env ChatEnv, req ChatCompletionRequest, result *sentinel.ChatResult, convID string) string {
	if !req.wantArtifactMarkdown() {
		return ""
	}
	var b strings.Builder

	if result.ExpectGeneratedImages {
		switch {
		case len(result.ImageFileIDs) > 0:
			for i, fileID := range result.ImageFileIDs {
				rel := fmt.Sprintf("/api/image/proxy?conv_id=%s&file_id=%s", convID, fileID)
				fmt.Fprintf(&b, "\n\n![Generated Image %d](%s)", i+1, env.absolute(rel))
			}
		case result.ImageFileID != "":
			rel := fmt.Sprintf("/api/image/proxy?conv_id=%s&file_id=%s", convID, result.ImageFileID)
			fmt.Fprintf(&b, "\n\n![Generated Image](%s)", env.absolute(rel))
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
		rel := fmt.Sprintf("/api/pdf/proxy?conv_id=%s&msg_id=%s&sandbox_path=%s",
			convID, f.MessageID, url.QueryEscape(f.SandboxPath))
		label := f.FileName
		if label == "" {
			label = fmt.Sprintf("file_%d", i+1)
		}
		fmt.Fprintf(&b, "\n\n[%s](%s)", label, env.absolute(rel))
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

	content := result.Text
	sentinel.LogContentPreview(func(format string, args ...interface{}) {
		fmt.Printf("[chat-response] "+format+"\n", args...)
	}, "client-body", content)

	content += e.artifactMarkdown(env, req, result, result.ConversationID)

	return &ChatCompletionResponse{
		ID:      turn.chatID,
		Object:  "chat.completion",
		Created: turn.createdAt,
		Model:   turn.apiModel,
		Choices: []Choice{{
			Index:            0,
			Message:          Message{Role: "assistant", Content: content},
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

	fmt.Printf("[chat-done] model=%s conv=%s expect_img=%v image_ids=%v %s text_len=%d streamed=%d\n",
		turn.apiModel, result.ConversationID, result.ExpectGeneratedImages, result.ImageFileIDs,
		result.ImageGenDiagSummary(), len(result.Text), streamedToClient.Len())

	// 兼容：可选 markdown 链接（旧客户端）
	if md := e.artifactMarkdown(env, req, result, registeredConvID); md != "" {
		emit(chunk(Delta{Content: md}, nil))
	}

	stopReason := "stop"
	stop := chunk(Delta{}, &stopReason)
	stop.ConversationID = registeredConvID
	emit(stop)
	return nil
}
