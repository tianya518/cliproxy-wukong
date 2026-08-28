package cliproxy

// errors.go —— 把 wukong 的错误翻译成 cliproxy 能正确处置的类型。
//
// 这一步不是锦上添花：cliproxy 默认把 executor 报错当成「这条凭证有问题」，
// 会给凭证记冷却甚至标记不可用。请求本身的问题（负载不合法、缺少输入）如果不
// 区分出来，一个坏客户端就能把整个凭证池打瘫。
//
// 对应两个接口：
//   - clipexec.StatusError        携带 HTTP 状态码，401 会触发换票重试
//   - clipexec.RequestScopedError 声明「错在请求不在凭证」，不影响凭证可用性

import (
	"errors"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/wukong/grok"
	sentinelserver "github.com/router-for-me/CLIProxyAPI/v7/wukong/server"
)

// requestError 请求自身的问题，与所选凭证无关。
type requestError struct{ err error }

func (e requestError) Error() string         { return e.err.Error() }
func (e requestError) Unwrap() error         { return e.err }
func (e requestError) IsRequestScoped() bool { return true }
func (e requestError) StatusCode() int       { return 400 }

// upstreamError 上游返回的错误，带状态码供凭证池判断是否换票。
type upstreamError struct {
	err  error
	code int
}

func (e upstreamError) Error() string   { return e.err.Error() }
func (e upstreamError) Unwrap() error   { return e.err }
func (e upstreamError) StatusCode() int { return e.code }

// classify 判断错误该按「请求问题」还是「凭证问题」上报。
func classify(err error) error {
	if err == nil {
		return nil
	}
	var re requestError
	if errors.As(err, &re) {
		return err
	}
	// 缺少可用输入是客户端拼错了请求，不该连累凭证。
	if errors.Is(err, sentinelserver.ErrNoInput) || errors.Is(err, grok.ErrNoInput) {
		return requestError{err}
	}
	if errors.Is(err, grok.ErrUnauthorized) {
		return upstreamError{err: err, code: 401}
	}
	if errors.Is(err, grok.ErrUsageLimit) {
		return upstreamError{err: err, code: 429}
	}
	if errors.Is(err, grok.ErrAntiBot) {
		return upstreamError{err: err, code: 403}
	}
	var gatewayStatus *grok.GatewayStatusError
	if errors.As(err, &gatewayStatus) && gatewayStatus.Soft() {
		return requestError{err}
	}
	if code := statusFromMessage(err.Error()); code > 0 {
		return upstreamError{err: err, code: code}
	}
	return err
}

// statusFromMessage 从上游错误文本里嗅探状态码。
//
// sentinel 侧的错误多是拼接出来的字符串而非结构化类型，这里只认几个会影响
// 凭证调度的关键码：401 换票、429 限流、402 额度。其余交给 cliproxy 默认处理。
func statusFromMessage(msg string) int {
	s := strings.ToLower(msg)
	switch {
	case strings.Contains(s, "401"),
		strings.Contains(s, "unauthorized"),
		strings.Contains(s, "invalid_token"),
		strings.Contains(s, "token expired"):
		return 401
	case strings.Contains(s, "429"), strings.Contains(s, "rate limit"), strings.Contains(s, "too many requests"):
		return 429
	case strings.Contains(s, "402"), strings.Contains(s, "quota"):
		return 402
	}
	return 0
}
