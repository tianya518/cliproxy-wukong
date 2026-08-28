package sentinel

// chat_ws.go —— WebSocket 通道相关：连接建立、topic 订阅循环、帧解析与消息分发。
//
// ChatGPT 一轮对话在 SSE 出现 stream_handoff 后，续流可能改走 WebSocket。这里负责：
//   - dialChatWS：拨号并订阅基础 topic；
//   - subscribeWSStream：订阅消费循环；
//   - processWSMessage / processWSEncodedItem / ingestWSMessageObject：把 WS 帧还原为 SSE 事件处理。
//
// 生图的收图主路径已改为 GET /conversation 轮询（见 image_revision.go），WS 主要承载文本 catchup。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// getWsURL 调用 celsius/ws/user 获取 WebSocket 连接地址
func (c *Client) getWsURL() (string, error) {
	resp, err := c.httpClient.R().
		SetHeaders(map[string]string{
			"Accept":                "*/*",
			"x-openai-target-path":  "/backend-api/celsius/ws/user",
			"x-openai-target-route": "/backend-api/celsius/ws/user",
		}).
		Get("/backend-api/celsius/ws/user")
	if err != nil {
		return "", fmt.Errorf("celsius/ws/user request: %w", err)
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("celsius/ws/user %d: %s", resp.StatusCode, truncateStr(resp.String(), 200))
	}
	var result struct {
		WebsocketURL string `json:"websocket_url"`
	}
	if err := json.Unmarshal(resp.Bytes(), &result); err != nil {
		return "", fmt.Errorf("parse celsius/ws/user: %w", err)
	}
	if result.WebsocketURL == "" {
		return "", fmt.Errorf("empty websocket_url")
	}
	return result.WebsocketURL, nil
}

// dialChatWS 获取 ws url 并完成握手+初始化订阅，返回已就绪的连接
func (c *Client) dialChatWS() (*websocket.Conn, error) {
	wsURL, err := c.getWsURL()
	if err != nil {
		return nil, err
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		NetDialContext:   c.dialContext,
	}
	hdrs := http.Header{}
	hdrs.Set("User-Agent", c.userAgent)
	hdrs.Set("Origin", "https://chatgpt.com")

	conn, _, err := dialer.Dial(wsURL, hdrs)
	if err != nil {
		return nil, fmt.Errorf("ws dial: %w", err)
	}

	// 初始化：connect + 订阅三个基础 topic
	initMsg := []map[string]interface{}{
		{"id": 1, "command": map[string]interface{}{
			"type":     "connect",
			"presence": map[string]string{"type": "presence", "state": "background"},
		}},
		{"id": 2, "command": map[string]interface{}{"type": "subscribe", "topic_id": "calpico-chatgpt"}},
		{"id": 3, "command": map[string]interface{}{"type": "subscribe", "topic_id": "conversations"}},
		{"id": 4, "command": map[string]interface{}{"type": "subscribe", "topic_id": "app_notifications"}},
	}
	if err := conn.WriteJSON(initMsg); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ws init send: %w", err)
	}

	// 不等待初始化 reply，由 subscribeWSStream 的读取循环统一处理所有帧
	return conn, nil
}

// wsIDCounter 用于 WebSocket 命令 id 自增（跨调用）
var wsIDCounter int64 = 4

func nextWsID() int64 {
	return atomic.AddInt64(&wsIDCounter, 1)
}

// parseWSFrames 将 WebSocket 文本帧解析为帧列表（支持 JSON 数组或单对象）
func parseWSFrames(raw []byte) []map[string]interface{} {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '[' {
		var frames []map[string]interface{}
		if err := json.Unmarshal(raw, &frames); err != nil {
			return nil
		}
		return frames
	}
	var single map[string]interface{}
	if err := json.Unmarshal(raw, &single); err != nil {
		return nil
	}
	return []map[string]interface{}{single}
}

func imageFileIDSeen(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// subscribeWSStream 通过已有 WebSocket 连接订阅 topic 并消费 encoded_item 里的 SSE 数据
func (c *Client) subscribeWSStream(conn *websocket.Conn, topicID string, result *ChatResult, opts ChatOptions, lastText *string, handler StreamHandler) error {
	subID := nextWsID()
	subMsg := []map[string]interface{}{
		{"id": subID, "command": map[string]interface{}{
			"type":     "subscribe",
			"topic_id": topicID,
			"offset":   "0",
		}},
	}
	if err := conn.WriteJSON(subMsg); err != nil {
		return fmt.Errorf("ws subscribe send: %w", err)
	}

	var useDeltaEncoding bool
	var currentEvent string
	done := false

	conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	defer conn.SetReadDeadline(time.Time{})

	for !done {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			// 正文已在 SSE/先前帧收齐时，WS 被代理或本机异常关闭不应再判整轮失败。
			if result != nil && !result.ExpectGeneratedImages {
				if strings.TrimSpace(result.assistantFinalText) != "" || result.bodyStreamFromSSE {
					c.logf("[ws] read error after final body ready, treat as done: %v", err)
					return nil
				}
			}
			return fmt.Errorf("ws read: %w", err)
		}

		conn.SetReadDeadline(time.Now().Add(120 * time.Second))

		frames := parseWSFrames(raw)
		if len(frames) == 0 {
			continue
		}

		for _, frame := range frames {
			fType, _ := frame["type"].(string)

			if fType == "reply" {
				reply, ok := frame["reply"].(map[string]interface{})
				if !ok {
					continue
				}
				replyTopicID, _ := reply["topic_id"].(string)
				if replyTopicID != topicID {
					continue
				}
				catchups, _ := reply["catchups"].([]interface{})
				if result.bodyStreamFromSSE && !result.ExpectGeneratedImages {
					c.logf("[ws] skip catchups=%d (final body already from HTTP SSE)", len(catchups))
					done = true
				} else {
					c.logf("[ws] reply catchups=%d", len(catchups))
					for _, cu := range catchups {
						if msg, ok := cu.(map[string]interface{}); ok {
							if c.processWSMessage(msg, result, opts, lastText, handler, &useDeltaEncoding, &currentEvent) {
								done = true
							}
						}
					}
				}
				continue
			}

			if fType == "message" {
				frameTopic, _ := frame["topic_id"].(string)
				if frameTopic != topicID {
					continue
				}
				if c.processWSMessage(frame, result, opts, lastText, handler, &useDeltaEncoding, &currentEvent) {
					done = true
				}
			}
		}
	}

	return nil
}

// processWSMessage 处理单条 WebSocket message 帧，返回 true 表示流结束
func (c *Client) processWSMessage(frame map[string]interface{}, result *ChatResult, opts ChatOptions, lastText *string, handler StreamHandler, useDeltaEncoding *bool, currentEvent *string) bool {
	payload1, ok := frame["payload"].(map[string]interface{})
	if !ok {
		c.probeUnhandledWSImageFrame(frame, result, "no_payload")
		return false
	}
	if payload2, ok := payload1["payload"].(map[string]interface{}); ok {
		if encoded, ok := payload2["encoded_item"].(string); ok && encoded != "" {
			return c.processWSEncodedItem(encoded, result, opts, lastText, handler, useDeltaEncoding, currentEvent)
		}
		if msg, ok := payload2["message"].(map[string]interface{}); ok {
			c.ingestWSMessageObject(msg, result, opts, handler, lastText, "ws_payload_message")
			return false
		}
	}
	if msg, ok := payload1["message"].(map[string]interface{}); ok {
		c.ingestWSMessageObject(msg, result, opts, handler, lastText, "ws_direct_message")
		return false
	}
	c.probeUnhandledWSImageFrame(frame, result, "no_encoded_item")
	return false
}

func (c *Client) ingestWSMessageObject(msg map[string]interface{}, result *ChatResult, opts ChatOptions, handler StreamHandler, lastText *string, via string) {
	if msg == nil || result == nil {
		return
	}
	result.noteTurnExchangeFromMessage(msg)
	if result.ExpectGeneratedImages {
		c.tryNoteGeneratedImagesFromMessage(msg, result, opts, via)
	}
	c.processConvUpdateMessage(msg, result, opts, handler, via)
	if author := getNestedString(msg, "author", "role"); author == "assistant" {
		if text := getFirstStringPart(msg); text != "" {
			channel, _ := msg["channel"].(string)
			if channel == "final" {
				c.emitBodyFull(result, lastText, text, "final", handler)
			}
		}
	}
}

func (c *Client) probeUnhandledWSImageFrame(frame map[string]interface{}, result *ChatResult, reason string) {
	if c == nil || frame == nil || result == nil || !result.ExpectGeneratedImages {
		return
	}
	raw, _ := json.Marshal(frame)
	s := string(raw)
	if !strings.Contains(s, "image_asset_pointer") && !strings.Contains(s, "sediment://") {
		return
	}
	fType, _ := frame["type"].(string)
	topic, _ := frame["topic_id"].(string)
	keys := []string{}
	if p1, ok := frame["payload"].(map[string]interface{}); ok {
		for k := range p1 {
			keys = append(keys, "p1."+k)
		}
		if p2, ok := p1["payload"].(map[string]interface{}); ok {
			for k := range p2 {
				keys = append(keys, "p2."+k)
			}
		}
	}
	c.logf("[image-ws][probe] 帧含图但未解析 reason=%s type=%s topic=%s keys=%v slots=%d",
		reason, fType, topic, keys, len(result.imageSlots))
}

func (c *Client) processWSEncodedItem(encoded string, result *ChatResult, opts ChatOptions, lastText *string, handler StreamHandler, useDeltaEncoding *bool, currentEvent *string) bool {
	for _, line := range strings.Split(encoded, "\n") {
		line = strings.TrimRight(line, "\r")

		if strings.HasPrefix(line, "event: ") {
			*currentEvent = strings.TrimSpace(line[7:])
			if *currentEvent == "delta_encoding" {
				*useDeltaEncoding = true
			}
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		ssePayload := strings.TrimSpace(line[6:])
		if ssePayload == "" || ssePayload == `"v1"` {
			continue
		}
		if ssePayload == "[DONE]" {
			return true
		}

		var evt map[string]interface{}
		if err := json.Unmarshal([]byte(ssePayload), &evt); err != nil {
			*currentEvent = ""
			continue
		}
		result.ArtifactSignals = MergeSignals(result.ArtifactSignals, ExtractSignalsFromJSON(evt))
		c.MergeApplyAndEmitArtifacts(result, opts)

		c.noteConversationID(result, opts, evt)

		evtType, _ := evt["type"].(string)
		if evtType == "resume_conversation_token" || evtType == "stream_handoff" {
			*currentEvent = ""
			continue
		}

		checkImageTaskID(evt, result)
		if *useDeltaEncoding && *currentEvent == "delta" {
			c.processDeltaSSE(evt, result, opts, lastText, handler)
		} else {
			c.processFullSSE(evt, result, opts, lastText, handler)
		}
		*currentEvent = ""
	}
	return false
}
