package server

// handler_chat_input.go —— 入站请求内容解析：从 OpenAI 风格 messages 里
// 提取最后一条 user 文本、system 提示词，以及多模态里的图片/文件（data URL / http URL）。

import "strings"

// parseMessageContent 解析多模态内容或纯文本内容
func parseMessageContent(c interface{}) (text string, images []string) {
	if c == nil {
		return
	}
	if s, ok := c.(string); ok {
		return s, nil
	}
	if arr, ok := c.([]interface{}); ok {
		for _, item := range arr {
			if m, ok := item.(map[string]interface{}); ok {
				t, _ := m["type"].(string)
				if t == "text" {
					if txt, ok := m["text"].(string); ok {
						text += txt
					}
				} else if t == "image_url" {
					if imgUrl, ok := m["image_url"].(map[string]interface{}); ok {
						if url, ok := imgUrl["url"].(string); ok {
							images = append(images, url)
						}
					}
				} else if t == "file" {
					if filePart, ok := m["file"].(map[string]interface{}); ok {
						if fileData, ok := filePart["file_data"].(string); ok && fileData != "" {
							// data:application/pdf;base64,... 格式，直接复用 data URL 通道
							images = append(images, fileData)
						}
					}
				}
			}
		}
	}
	return
}

// extractUserMessage 从 messages 中提取最后一条 user 消息和 system 提示词
func extractUserMessage(messages []Message) (userMsg string, systemPrompt string, images []string) {
	// 找 system prompt
	for _, m := range messages {
		if strings.ToLower(m.Role) == "system" {
			systemPrompt, _ = parseMessageContent(m.Content)
			break
		}
	}
	// 找最后一条 user 消息
	if i := lastUserIndex(messages); i >= 0 {
		userMsg, images = parseMessageContent(messages[i].Content)
	}
	return
}

// lastUserIndex 返回最后一条 user 消息的下标，没有则返回 -1。
func lastUserIndex(messages []Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.ToLower(messages[i].Role) == "user" {
			return i
		}
	}
	return -1
}

// flattenHistory 把最后一条 user 消息之前的历史轮次整理成纯文本记录。
//
// 每轮只向上游发送一条消息：带 conversation_id 时上下文由 ChatGPT 侧的会话维持，
// 但标准 OpenAI 客户端（cliproxy 等）是无状态的，只会重发完整 messages 数组、
// 不会回传 conversation_id。那种情况下必须把历史一并展平进本轮输入，
// 否则除最后一条以外的消息会被整段丢弃。
func flattenHistory(messages []Message) string {
	last := lastUserIndex(messages)
	if last <= 0 {
		return ""
	}

	var b strings.Builder
	for _, m := range messages[:last] {
		role := strings.ToLower(m.Role)
		if role != "user" && role != "assistant" {
			continue
		}
		text, _ := parseMessageContent(m.Content)
		if text = strings.TrimSpace(text); text == "" {
			continue
		}
		label := "User"
		if role == "assistant" {
			label = "Assistant"
		}
		b.WriteString(label)
		b.WriteString(": ")
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}
