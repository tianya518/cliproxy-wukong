package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type ChatRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	Stream         bool            `json:"stream"`
	ConversationID string          `json:"conversation_id,omitempty"`
	ParentID       string          `json:"parent_id,omitempty"`
	Size           string          `json:"size,omitempty"`
	N              int             `json:"n,omitempty"`
	Tools          json.RawMessage `json:"tools,omitempty"`
	ToolChoice     json.RawMessage `json:"tool_choice,omitempty"`
}

type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type ChatResult struct {
	ID             string
	Model          string
	ConversationID string
	ParentID       string
	Text           string
	Reasoning      string
	Images         []string
	VideoURL       string
	ToolCalls      []parsedToolCall
	SearchSources  []map[string]any
	FinishReason   string
}

type StreamDelta struct {
	Kind  string
	Text  string
	Image string
}

func (c *Client) Complete(ctx context.Context, req ChatRequest) (*ChatResult, error) {
	return c.runTurn(ctx, req, nil)
}

func (c *Client) Stream(ctx context.Context, req ChatRequest, emit func(StreamDelta)) (*ChatResult, error) {
	return c.runTurn(ctx, req, emit)
}

func (c *Client) runTurn(ctx context.Context, req ChatRequest, emit func(StreamDelta)) (*ChatResult, error) {
	if strings.TrimSpace(req.Model) == "" {
		req.Model = "grok-chat-fast"
	}
	spec, ok := Resolve(req.Model)
	if !ok {
		return nil, fmt.Errorf("未知 Grok Web 模型 %q", req.Model)
	}
	messages, err := encodeMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	result := &ChatResult{ID: "chatcmpl-" + newWebID("grok"), Model: spec.PublicID, FinishReason: "stop"}

	switch spec.Capability {
	case CapabilityImage:
		input, normErr := normalizeLatestImageInput(messages)
		if normErr != nil {
			return nil, normErr
		}
		if hasImageAttachments(input.Attachments) {
			return nil, fmt.Errorf("文生图模型只接受当前用户消息中的纯文本；图生图请使用 grok-imagine-image-edit")
		}
		images, genErr := c.GenerateImage(ctx, spec.PublicID, input.Prompt, req.Size, req.N)
		if genErr != nil {
			return nil, genErr
		}
		result.Images = images.URLs
		result.Text = imageMarkdown(images.URLs)
		if emit != nil {
			emit(StreamDelta{Kind: "text", Text: result.Text})
		}
		return result, nil
	case CapabilityImageEdit:
		input, normErr := normalizeLatestImageInput(messages)
		if normErr != nil {
			return nil, normErr
		}
		urls := make([]string, 0, len(input.Attachments))
		for _, att := range input.Attachments {
			if att.Image {
				urls = append(urls, att.Source)
			}
		}
		if len(urls) == 0 {
			return nil, fmt.Errorf("图生图请在当前用户消息里提供 image_url，模型使用 grok-imagine-image-edit")
		}
		images, editErr := c.EditImage(ctx, input.Prompt, urls, req.Size)
		if editErr != nil {
			return nil, editErr
		}
		result.Images = images.URLs
		result.Text = imageMarkdown(images.URLs)
		if emit != nil {
			emit(StreamDelta{Kind: "text", Text: result.Text})
		}
		return result, nil
	case CapabilityVideo:
		input, normErr := normalizeLatestImageInput(messages)
		if normErr != nil {
			return nil, normErr
		}
		video, vidErr := c.GenerateVideo(ctx, videoRequestFromInput(input, req.Size))
		if vidErr != nil {
			return nil, vidErr
		}
		result.VideoURL = video.URL
		result.ConversationID = video.Conversation
		result.Text = "![Generated Video](" + video.URL + ")"
		if emit != nil {
			emit(StreamDelta{Kind: "text", Text: result.Text})
		}
		return result, nil
	}

	input, err := normalizeOpenAIInput(messages)
	if err != nil {
		return nil, err
	}
	tools, err := parseToolConfiguration(req.Tools, req.ToolChoice)
	if err != nil {
		return nil, err
	}
	input.Prompt = injectToolPrompt(input.Prompt, tools)
	ids, err := c.prepareChatAttachments(ctx, input.Attachments)
	if err != nil {
		return nil, err
	}
	previous := c.turnState(req)
	source, err := c.openGatewayStream(ctx, spec, input.Prompt, ids, previous)
	if err != nil {
		return nil, err
	}
	defer source.Close()

	var parsed parsedChat
	var sieve *toolStreamSieve
	if len(tools.Functions) > 0 && tools.Choice != "none" {
		sieve = newToolStreamSieve(tools.available)
	}
	err = consumeUpstreamInto(source, &parsed, func(kind, delta string) error {
		if emit == nil {
			return nil
		}
		if kind == "image" {
			emit(StreamDelta{Kind: "image", Image: delta})
			return nil
		}
		if kind == "text" && sieve != nil {
			fed := sieve.Feed(delta)
			if fed.SafeText != "" {
				emit(StreamDelta{Kind: "text", Text: fed.SafeText})
			}
			if fed.Complete && len(fed.Calls) > 0 {
				parsed.ToolCalls = fed.Calls
			}
			return nil
		}
		if delta != "" {
			emit(StreamDelta{Kind: kind, Text: delta})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if sieve != nil && len(parsed.ToolCalls) == 0 {
		flushed := sieve.Flush()
		if flushed.SafeText != "" && emit != nil {
			emit(StreamDelta{Kind: "text", Text: flushed.SafeText})
		}
		if len(flushed.Calls) > 0 {
			parsed.ToolCalls = flushed.Calls
		}
	}
	if parsed.ConversationID != "" {
		c.conversationID = parsed.ConversationID
	}
	if parsed.ParentID != "" {
		c.parentResponseID = parsed.ParentID
	}
	result.ConversationID = parsed.ConversationID
	result.ParentID = parsed.ParentID
	result.Text = parsed.Text.String()
	result.Reasoning = parsed.Reasoning.String()
	result.Images = parsed.Images
	result.ToolCalls = parsed.ToolCalls
	result.SearchSources = parsed.SearchSources
	if len(parsed.ToolCalls) > 0 {
		result.FinishReason = "tool_calls"
	}
	if result.Text == "" && len(result.Images) > 0 {
		result.Text = imageMarkdown(result.Images)
	}
	return result, nil
}

func (c *Client) turnState(req ChatRequest) *TurnState {
	id := strings.TrimSpace(req.ConversationID)
	if id == "" {
		return nil
	}
	parent := strings.TrimSpace(req.ParentID)
	if parent == "" && id == strings.TrimSpace(c.conversationID) {
		parent = c.parentResponseID
	}
	return &TurnState{ConversationID: id, ParentID: parent}
}

func encodeMessages(messages []Message) ([]chatMessage, error) {
	out := make([]chatMessage, 0, len(messages))
	for _, message := range messages {
		raw, err := json.Marshal(message.Content)
		if err != nil {
			return nil, err
		}
		out = append(out, chatMessage{Role: message.Role, Content: raw})
	}
	return out, nil
}

func imageMarkdown(urls []string) string {
	var b strings.Builder
	for i, raw := range urls {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "![Generated Image %d](%s)", i+1, raw)
	}
	return b.String()
}

func OpenAIChunk(id, model, role, content string, finish *string, conversationID string) map[string]any {
	delta := map[string]any{}
	if role != "" {
		delta["role"] = role
	}
	if content != "" {
		delta["content"] = content
	}
	chunk := map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if conversationID != "" {
		chunk["conversation_id"] = conversationID
	}
	return chunk
}

func OpenAICompletion(result *ChatResult) map[string]any {
	message := map[string]any{"role": "assistant", "content": result.Text}
	if result.Reasoning != "" {
		message["reasoning_content"] = result.Reasoning
	}
	if len(result.ToolCalls) > 0 {
		calls := make([]any, 0, len(result.ToolCalls))
		for i, call := range result.ToolCalls {
			calls = append(calls, map[string]any{
				"id": call.ID, "type": "function", "index": i,
				"function": map[string]any{"name": call.Name, "arguments": call.Arguments},
			})
		}
		message["tool_calls"] = calls
	}
	return map[string]any{
		"id": result.ID, "object": "chat.completion", "created": time.Now().Unix(), "model": result.Model,
		"choices": []any{map[string]any{
			"index": 0, "message": message, "finish_reason": result.FinishReason,
			"reasoning_content": result.Reasoning,
		}},
		"usage":           map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
		"conversation_id": result.ConversationID,
	}
}
