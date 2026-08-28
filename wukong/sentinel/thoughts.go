package sentinel

// thoughts.go —— thinking 模型的"思考步骤"解析：
//   - extractThoughts：从 SSE 的 content_type="thoughts" 增量里抽取已完成步骤并去重推送。

// extractThoughts 从 content_type="thoughts" 消息的 thoughts 数组中提取已完成的思考步骤。
// SSE 流中的数组元素格式：{"summary": "...", "content": "...", "chunks": [...], "finished": true}
// 每个 finished=true 的步骤通过 \x00THINK_STEP\x00 标记推送一次（summary\x1Fcontent），去重处理。
func (c *Client) extractThoughts(thoughts []interface{}, result *ChatResult, handler StreamHandler) {
	if result.seenThoughtKeys == nil {
		result.seenThoughtKeys = make(map[string]bool)
	}
	for _, tRaw := range thoughts {
		t, ok := tRaw.(map[string]interface{})
		if !ok {
			continue
		}
		// SSE 格式：直接包含 summary, content, finished
		finished, _ := t["finished"].(bool)
		if !finished {
			continue
		}
		summary, _ := t["summary"].(string)
		content, _ := t["content"].(string)
		if summary == "" {
			continue
		}
		// 去重：同一个 summary 只推送一次
		if result.seenThoughtKeys[summary] {
			continue
		}
		result.seenThoughtKeys[summary] = true
		result.ThinkSteps = append(result.ThinkSteps, ThinkStep{Summary: summary, Content: content})
		c.logf("[thoughts] 新思考步骤: %s", summary)
		if handler != nil {
			payload := summary
			if content != "" {
				payload += "\x1F" + content
			}
			handler("\x00THINK_STEP\x00" + payload)
		}
	}
}
