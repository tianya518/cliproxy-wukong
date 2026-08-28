package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	gatewayHandshakeTimeout = 20 * time.Second
	gatewayHeartbeatPeriod  = 25 * time.Second
	gatewayMaxFrameBytes    = 16 << 20
)

type gatewayEnvelope struct {
	SessionID string `json:"session_id"`
	Event     struct {
		Type          string `json:"type"`
		ClientEventID string `json:"client_event_id"`
		Conversation  struct {
			ID string `json:"id"`
		} `json:"conversation"`
	} `json:"event"`
}

type gatewaySender struct {
	mu         sync.Mutex
	connection *websocket.Conn
}

func (s *gatewaySender) write(value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.connection.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return s.connection.WriteJSON(value)
}

func gatewayEndpoint(baseURL, userID string) (string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" {
		return "", "", fmt.Errorf("Grok Web Base URL 无效")
	}
	origin := (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String()
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	default:
		return "", "", fmt.Errorf("Grok Web Base URL 协议无效")
	}
	parsed.Path = "/ws/mgw/"
	parsed.RawPath = ""
	parsed.RawQuery = url.Values{"uid": []string{userID}}.Encode()
	parsed.Fragment = ""
	return parsed.String(), origin, nil
}

func gatewayHeaders(origin, userID, token, cfCookies, userAgent string) http.Header {
	headers := http.Header{}
	headers.Set("Origin", origin)
	headers.Set("User-Agent", userAgent)
	headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Pragma", "no-cache")
	headers.Set("Cookie", BuildSSOCookie(token, cfCookies)+"; x-userid="+userID)
	return headers
}

func gatewaySession(model string, previous *TurnState) map[string]any {
	xGrok := map[string]any{
		"protocol_capabilities":   []string{"conversation_attached", "custom_methods_v1"},
		"use_chunk":               true,
		"enable_side_by_side":     true,
		"force_side_by_side":      false,
		"enable_image_generation": true,
		"image_generation_count":  2,
		"disable_text_follow_ups": false,
		"disable_artifact":        true,
		"force_concise":           false,
	}
	if previous == nil || previous.ConversationID == "" {
		xGrok["keep_context"] = false
		xGrok["is_temporary"] = true
		xGrok["disable_memory"] = true
	} else {
		xGrok["conversation_id"] = previous.ConversationID
		xGrok["load_existing"] = true
		xGrok["needs_history"] = false
	}
	return map[string]any{"model": model, "x_grok": xGrok}
}

func gatewayTurnEvents(sessionID, prompt string, attachments []string, previous *TurnState) (map[string]any, map[string]any) {
	chunks := make([]any, 0, len(attachments)+1)
	for _, attachment := range attachments {
		chunks = append(chunks, map[string]any{"mention": map[string]any{"target": map[string]any{"file_mention": map[string]any{"file_id": attachment}}}})
	}
	chunks = append(chunks, map[string]any{"text": map[string]any{"text": prompt}})
	item := map[string]any{
		"type": "message", "role": "user",
		"x_grok": map[string]any{"client_message_id": newRequestUUID(), "input_chunks": chunks},
	}
	if len(attachments) > 0 {
		item["file_attachment_ids"] = attachments
	}
	now := time.Now().UnixMilli()
	itemEvent := map[string]any{
		"session_id": sessionID,
		"event": map[string]any{
			"type": "conversation.item.create", "event_id": fmt.Sprintf("evt_msg_%d", now), "item": item,
		},
	}
	if previous != nil && previous.ParentID != "" {
		itemEvent["event"].(map[string]any)["parent_response_id"] = previous.ParentID
	}
	if len(attachments) > 0 {
		itemEvent["event"].(map[string]any)["file_attachment_ids"] = attachments
	}
	responseEvent := map[string]any{
		"session_id": sessionID,
		"event":      map[string]any{"type": "response.create", "event_id": fmt.Sprintf("evt_resp_%d", now)},
	}
	return itemEvent, responseEvent
}

func (c *Client) openGatewayStream(ctx context.Context, spec ModelSpec, prompt string, attachments []string, previous *TurnState) (io.ReadCloser, error) {
	userID, err := c.gatewayUserID(ctx)
	if err != nil {
		return nil, err
	}
	endpoint, origin, err := gatewayEndpoint(c.baseURL(), userID)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.cfg.ChatTimeout)
	headers := gatewayHeaders(origin, userID, c.cred.AccessToken(), c.cloudflareCookies(), c.userAgent())
	connection, handshake, err := c.dialWS(requestCtx, endpoint, headers)
	if err != nil {
		cancel()
		if handshake != nil {
			return nil, readMediaStatusError("Grok Gateway 握手", handshake)
		}
		return nil, fmt.Errorf("连接 Grok Gateway: %w", err)
	}
	reader, writer := io.Pipe()
	go func() {
		<-requestCtx.Done()
		_ = connection.Close()
	}()
	go func() {
		streamErr := runGatewayStream(requestCtx, connection, writer, spec.Mode, prompt, attachments, previous)
		cancel()
		_ = connection.Close()
		_ = writer.CloseWithError(streamErr)
	}()
	return reader, nil
}

func runGatewayStream(ctx context.Context, connection *websocket.Conn, writer io.Writer, model, prompt string, attachments []string, previous *TurnState) error {
	connection.SetReadLimit(gatewayMaxFrameBytes)
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetReadDeadline(deadline)
	}
	sender := &gatewaySender{connection: connection}
	initialEventID := "evt_init_" + newRequestUUID()
	initial := map[string]any{
		"event": map[string]any{
			"type": "session.create", "event_id": initialEventID,
			"session": gatewaySession(model, previous),
		},
	}
	currentSessionID := ""
	if previous != nil {
		currentSessionID = previous.ConversationID
		if currentSessionID != "" {
			initial["session_id"] = currentSessionID
		}
	}
	if err := sender.write(initial); err != nil {
		return fmt.Errorf("发送 Grok Gateway session.create: %w", err)
	}
	heartbeatDone := make(chan struct{})
	defer close(heartbeatDone)
	go gatewayHeartbeat(sender, heartbeatDone)
	created := false
	attached := false
	turnSent := false
	for {
		messageType, data, err := connection.ReadMessage()
		if err != nil {
			if isGatewayNormalClose(err) {
				return nil
			}
			return fmt.Errorf("读取 Grok Gateway: %w", err)
		}
		if messageType != websocket.TextMessage {
			continue
		}
		if len(data) > gatewayMaxFrameBytes {
			return fmt.Errorf("Grok Gateway 响应帧超过安全上限")
		}
		var envelope gatewayEnvelope
		if err := json.Unmarshal(data, &envelope); err != nil {
			continue
		}
		if _, err := writer.Write(append(append([]byte(nil), data...), '\n')); err != nil {
			return err
		}
		switch envelope.Event.Type {
		case "session.created":
			if envelope.Event.ClientEventID != "" && envelope.Event.ClientEventID != initialEventID {
				continue
			}
			created = true
			if currentSessionID == "" {
				currentSessionID = envelope.SessionID
			}
		case "conversation.attached":
			attached = true
			if currentSessionID == "" {
				currentSessionID = envelope.Event.Conversation.ID
			}
			if envelope.Event.Conversation.ID == "" || envelope.Event.Conversation.ID != currentSessionID {
				return fmt.Errorf("Grok Gateway 返回了不一致的 conversation id")
			}
		case "response.done", "error":
			return nil
		case "session.ended":
			return fmt.Errorf("Grok Gateway session 在响应完成前结束")
		}
		if created && attached && !turnSent {
			turnSent = true
			item, response := gatewayTurnEvents(currentSessionID, prompt, attachments, previous)
			if err := sender.write(item); err != nil {
				return fmt.Errorf("发送 Grok Gateway conversation.item.create: %w", err)
			}
			if err := sender.write(response); err != nil {
				return fmt.Errorf("发送 Grok Gateway response.create: %w", err)
			}
		}
	}
}

func isGatewayNormalClose(err error) bool {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		return closeErr.Code == websocket.CloseNormalClosure || closeErr.Code == websocket.CloseNoStatusReceived
	}
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed)
}

func gatewayHeartbeat(sender *gatewaySender, done <-chan struct{}) {
	ticker := time.NewTicker(gatewayHeartbeatPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-ticker.C:
			if err := sender.write(map[string]any{"event": map[string]any{"type": "ping", "event_id": fmt.Sprintf("evt_hb_%d", now.UnixMilli())}}); err != nil {
				_ = sender.connection.Close()
				return
			}
		}
	}
}
