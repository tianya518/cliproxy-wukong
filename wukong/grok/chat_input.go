package grok

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type chatMessage struct {
	Type       string          `json:"type"`
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  json.RawMessage `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	CallID     string          `json:"call_id"`
	Name       string          `json:"name"`
	Arguments  string          `json:"arguments"`
	Output     json.RawMessage `json:"output"`
}

func normalizeOpenAIInput(messages []chatMessage) (normalizedChatInput, error) {
	if len(messages) == 0 {
		return normalizedChatInput{}, errors.New("messages 不能为空")
	}
	var builder strings.Builder
	attachments := make([]chatAttachmentInput, 0, 2)
	for _, message := range messages {
		typeName := strings.ToLower(strings.TrimSpace(message.Type))
		if typeName == "function_call" {
			if !toolNamePattern.MatchString(strings.TrimSpace(message.Name)) {
				return normalizedChatInput{}, errors.New("function_call.name 无效")
			}
			arguments := normalizeToolArguments(message.Arguments)
			if !json.Valid([]byte(arguments)) {
				return normalizedChatInput{}, errors.New("function_call.arguments 必须是有效 JSON")
			}
			builder.WriteString("[assistant]\n<tool_calls>\n  <tool_call>\n    <tool_name>")
			builder.WriteString(message.Name)
			builder.WriteString("</tool_name>\n    <parameters>")
			builder.WriteString(arguments)
			builder.WriteString("</parameters>\n  </tool_call>\n</tool_calls>\n\n")
			continue
		}
		if typeName == "function_call_output" {
			text, err := rawTextValue(message.Output)
			if err != nil {
				return normalizedChatInput{}, errors.New("function_call_output.output 必须是字符串或 JSON")
			}
			builder.WriteString("[tool result for ")
			builder.WriteString(strings.TrimSpace(message.CallID))
			builder.WriteString("]\n")
			builder.WriteString(text)
			builder.WriteString("\n\n")
			continue
		}
		text, messageAttachments, err := contentTextAndAttachments(message.Content)
		if err != nil {
			return normalizedChatInput{}, err
		}
		attachments = append(attachments, messageAttachments...)
		if len(message.ToolCalls) > 0 {
			xml := toolCallsToXML(message.ToolCalls)
			if text != "" && xml != "" {
				text += "\n" + xml
			} else if xml != "" {
				text = xml
			}
		}
		if message.ToolCallID != "" {
			text = "Tool result (" + message.ToolCallID + "): " + text
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		builder.WriteString("[")
		builder.WriteString(strings.ToLower(strings.TrimSpace(message.Role)))
		builder.WriteString("]\n")
		builder.WriteString(text)
		builder.WriteString("\n\n")
	}
	value := strings.TrimSpace(builder.String())
	if value == "" && len(attachments) == 0 {
		return normalizedChatInput{}, ErrNoInput
	}
	if len(attachments) > maxChatAttachments {
		return normalizedChatInput{}, fmt.Errorf("单次对话最多支持 %d 个附件", maxChatAttachments)
	}
	return normalizedChatInput{Prompt: value, Attachments: attachments}, nil
}

func hasImageAttachments(attachments []chatAttachmentInput) bool {
	for _, attachment := range attachments {
		if attachment.Image && strings.TrimSpace(attachment.Source) != "" {
			return true
		}
	}
	return false
}

func normalizeLatestImageInput(messages []chatMessage) (normalizedChatInput, error) {
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if !strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			continue
		}
		prompt, attachments, err := contentTextAndAttachments(message.Content)
		if err != nil {
			return normalizedChatInput{}, err
		}
		prompt = strings.TrimSpace(prompt)
		if prompt == "" && len(attachments) == 0 {
			return normalizedChatInput{}, errors.New("图片生成提示词不能为空")
		}
		return normalizedChatInput{Prompt: prompt, Attachments: attachments}, nil
	}
	return normalizedChatInput{}, errors.New("messages 中缺少用户消息")
}

func rawTextValue(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}
	var text string
	if json.Unmarshal(trimmed, &text) == nil {
		return text, nil
	}
	if !json.Valid(trimmed) {
		return "", errors.New("invalid JSON")
	}
	return string(trimmed), nil
}

func contentTextAndAttachments(raw json.RawMessage) (string, []chatAttachmentInput, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil, nil
	}
	if trimmed[0] == '"' {
		var value string
		if json.Unmarshal(trimmed, &value) != nil {
			return "", nil, errors.New("消息 content 字符串无效")
		}
		return value, nil, nil
	}
	var parts []map[string]any
	if json.Unmarshal(trimmed, &parts) != nil {
		return "", nil, errors.New("消息 content 必须是字符串或内容数组")
	}
	values := make([]string, 0, len(parts))
	attachments := make([]chatAttachmentInput, 0, 2)
	for _, part := range parts {
		typeName, _ := part["type"].(string)
		switch typeName {
		case "text", "input_text", "output_text":
			if text, _ := part["text"].(string); text != "" {
				values = append(values, text)
			}
		case "image_url", "input_image", "image":
			if value := extractImageURL(part); value != "" {
				attachments = append(attachments, chatAttachmentInput{Source: value, Image: true})
			} else {
				return "", nil, errors.New("图片内容缺少 image_url")
			}
		case "file", "input_file":
			attachment, err := extractFileAttachment(part)
			if err != nil {
				return "", nil, err
			}
			attachments = append(attachments, attachment)
		case "input_audio":
			return "", nil, errors.New("Grok Web 对话暂不支持 input_audio 内容")
		default:
			return "", nil, fmt.Errorf("Grok Web 对话暂不支持 content.type=%q", typeName)
		}
	}
	return strings.Join(values, "\n"), attachments, nil
}

func extractFileAttachment(part map[string]any) (chatAttachmentInput, error) {
	value := part
	if nested, _ := part["file"].(map[string]any); nested != nil {
		value = nested
	}
	fileURL, _ := value["file_url"].(string)
	fileData, _ := value["file_data"].(string)
	source := strings.TrimSpace(firstNonEmpty(fileURL, fileData))
	if source == "" {
		return chatAttachmentInput{}, errors.New("input_file 缺少 file_url 或 file_data")
	}
	filename, _ := value["filename"].(string)
	return chatAttachmentInput{Source: source, Filename: strings.TrimSpace(filename)}, nil
}

func extractImageURL(part map[string]any) string {
	value := part["image_url"]
	if text, ok := value.(string); ok {
		return text
	}
	if object, ok := value.(map[string]any); ok {
		text, _ := object["url"].(string)
		return text
	}
	return ""
}
