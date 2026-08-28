package grok

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

type mediaStatusError struct {
	op     string
	status int
	body   []byte
}

func (e *mediaStatusError) Error() string {
	if e == nil {
		return ""
	}
	summary := strings.TrimSpace(string(e.body))
	if len(summary) > 240 {
		summary = summary[:240] + "…"
	}
	if summary == "" {
		return fmt.Sprintf("%s返回 %d", e.op, e.status)
	}
	return fmt.Sprintf("%s返回 %d: %s", e.op, e.status, summary)
}

func newMediaStatusError(op string, status int, body []byte) *mediaStatusError {
	return &mediaStatusError{op: op, status: status, body: append([]byte(nil), body...)}
}

func readMediaStatusError(op string, resp *http.Response) *mediaStatusError {
	if resp == nil {
		return newMediaStatusError(op, 0, nil)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, webMediaDiagnosticBodyLimit))
	_ = resp.Body.Close()
	return newMediaStatusError(op, resp.StatusCode, body)
}

type webMediaUpstreamError struct {
	status              int
	bodyKind            string
	cloudflareChallenge bool
}

func newWebMediaUpstreamError(status int, body []byte) *webMediaUpstreamError {
	return &webMediaUpstreamError{
		status:              status,
		bodyKind:            classifyWebMediaDiagnosticBody(body),
		cloudflareChallenge: isCloudflareChallengeBody(body),
	}
}

func classifyWebMediaDiagnosticBody(body []byte) string {
	if !utf8.Valid(body) {
		return "binary"
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "empty"
	}
	if json.Valid(body) || json.Valid([]byte(trimmed)) {
		return "json"
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html") {
		return "html"
	}
	for _, value := range trimmed {
		if value < 0x20 && value != '\t' && value != '\r' && value != '\n' {
			return "binary"
		}
	}
	return "text"
}

func isCloudflareChallengeBody(body []byte) bool {
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "just a moment") ||
		strings.Contains(lower, "challenge-platform") ||
		strings.Contains(lower, "__cf_chl") ||
		strings.Contains(lower, "cf-chl-")
}

func isClearanceRefreshableMediaError(e *webMediaUpstreamError) bool {
	if e == nil || e.status != http.StatusForbidden {
		return false
	}
	return e.cloudflareChallenge || e.bodyKind == "empty" || e.bodyKind == "html"
}

func isStatsigRefreshableMediaError(e *webMediaUpstreamError, body []byte) bool {
	if e == nil || e.status != http.StatusForbidden || e.bodyKind != "json" || isDefinitiveAccountBlockBody(body) {
		return false
	}
	code, message, structured := extractWebMediaUpstreamErrorFields(body)
	if !structured {
		return false
	}
	normalized := strings.ToLower(message)
	return code == "7" || strings.Contains(normalized, "anti-bot") ||
		strings.Contains(normalized, "page is out of date") || strings.Contains(normalized, "reload to continue")
}

func isDefinitiveAccountBlockBody(body []byte) bool {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return isDefinitiveAccountBlockText(string(body))
	}
	values := []string{
		firstString(payload, "code", "message", "error"),
	}
	if nested, ok := payload["error"].(map[string]any); ok {
		values = append(values, firstString(nested, "code", "message", "error"))
	}
	return isDefinitiveAccountBlockText(strings.Join(values, " "))
}

func isDefinitiveAccountBlockText(value string) bool {
	value = strings.ToLower(value)
	return strings.Contains(value, "blocked-user") || strings.Contains(value, "user is blocked")
}

func extractWebMediaUpstreamErrorFields(body []byte) (code, message string, structured bool) {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return "", "", false
	}
	if errorObject, ok := root["error"].(map[string]any); ok {
		code = firstWebMediaDiagnosticCode(errorObject, "code", "type", "error")
		message = firstString(errorObject, "message", "error", "detail")
	} else if errorText, ok := root["error"].(string); ok {
		message = errorText
	}
	if code == "" {
		code = firstWebMediaDiagnosticCode(root, "code", "error_code", "type")
	}
	if message == "" {
		message = firstString(root, "message", "error_message", "detail")
	}
	return code, message, true
}

func firstWebMediaDiagnosticCode(value map[string]any, keys ...string) string {
	if code := firstString(value, keys...); code != "" {
		return code
	}
	if code, ok := firstInt(value, keys...); ok {
		return fmt.Sprintf("%d", code)
	}
	return ""
}

func isPageOutOfDate(status int, body []byte) bool {
	return isStatsigRefreshableMediaError(newWebMediaUpstreamError(status, body), body)
}

func classifyMediaError(err error) (*mediaStatusError, *webMediaUpstreamError, bool) {
	var mediaErr *mediaStatusError
	if err == nil || !errorAsMedia(err, &mediaErr) {
		return nil, nil, false
	}
	return mediaErr, newWebMediaUpstreamError(mediaErr.status, mediaErr.body), true
}

func errorAsMedia(err error, out **mediaStatusError) bool {
	if err == nil {
		return false
	}
	if typed, ok := err.(*mediaStatusError); ok {
		*out = typed
		return true
	}
	return false
}
