package sentinel

// image_ws.go —— 生图消息的 conversation-update 解析与收尾定稿。
//
// 网页端生图会在 conversation-update.message 里逐步带出 image_asset_pointer。
// 本文件负责解析这些消息、驱动槽位修订，并在收尾时定稿（FinishImageGenWS）。
// 注意：当前收图主路径是 GET /conversation 轮询（见 image_handoff.go / image_revision.go）。

import (
	"strings"
)

// FinishImageGenWS 生图 WS 结束或 HTTP 收尾：定稿各槽位并刷新 ImageFileIDs。
func (c *Client) FinishImageGenWS(result *ChatResult, opts ChatOptions) {
	if result == nil || !result.ExpectGeneratedImages {
		return
	}
	c.FinalizeImageGenSlots(result, opts)
	result.RebuildImageFileIDsFromSlots()
}

func (c *Client) processConvUpdateMessage(msg map[string]interface{}, result *ChatResult, opts ChatOptions, handler StreamHandler, wsUpdateType string) {
	msgID, _ := msg["id"].(string)
	if result.ExpectGeneratedImages {
		c.tryNoteGeneratedImagesFromMessage(msg, result, opts, wsUpdateType)
		if meta, ok := msg["metadata"].(map[string]interface{}); ok {
			if refs, ok := meta["content_references"].([]interface{}); ok {
				for _, refRaw := range refs {
					ref, _ := refRaw.(map[string]interface{})
					if ap, _ := ref["asset_pointer"].(string); ap != "" {
						if fileID := extractFileID(ap); fileID != "" {
							c.logf("[image-ws] content_reference asset: %s", fileID)
							c.noteGeneratedImageRevision(result, opts, ParsedGeneratedImage{
								FileID: fileID, MessageID: msgID,
							}, wsUpdateType)
						}
					}
				}
			}
		}
	}
	author, _ := msg["author"].(map[string]interface{})
	role, _ := author["role"].(string)
	channel, _ := msg["channel"].(string)
	msgContent, _ := msg["content"].(map[string]interface{})
	parts, _ := msgContent["parts"].([]interface{})

	if channel == "analysis" {
		for _, part := range parts {
			if text, ok := part.(string); ok && text != "" {
				if handler != nil {
					handler(text)
				}
			}
		}
		return
	}

	// thinking 批量生图：图像工具（recipient 如 t2uay3k.sj1i4kz）的工具名不含 dalle/image_gen，
	// 但其 code 节点携带 batch_requests。据内容识别，置 sawImageGenTool 以驱动后续轮询收图。
	if ct, _ := msgContent["content_type"].(string); ct == "code" {
		if txt, _ := msgContent["text"].(string); strings.Contains(txt, "batch_requests") {
			if !result.sawImageGenTool {
				c.logf("[image-route][ws] 检测到 batch_requests code 节点 → 判为生图轮次")
			}
			result.sawImageGenTool = true
			result.ExpectGeneratedImages = true
		}
	}

	if role == "tool" {
		name, _ := author["name"].(string)
		lowerName := strings.ToLower(name)
		if strings.Contains(lowerName, "dalle") || strings.Contains(lowerName, "image_gen") {
			result.sawImageGenTool = true
		}
		status, _ := msg["status"].(string)
		isImageTool := strings.Contains(lowerName, "dalle") || strings.Contains(lowerName, "image_gen")
		if isImageTool && status == "in_progress" && !result.DalleStarted {
			title := "正在生成图片，请稍候..."
			for _, p := range parts {
				if pStr, ok := p.(string); ok && pStr != "" {
					title = "正在生成图片: " + pStr
					break
				}
			}
			opts.Artifacts.normalized().emit(StreamEvent{
				Event: StreamEventArtifactPending,
				Kind:  "generated_image",
				Title: title,
			})
			if handler != nil {
				handler("\n\n[" + title + "...]\n\n")
			}
			result.DalleStarted = true
		}
	}
}
