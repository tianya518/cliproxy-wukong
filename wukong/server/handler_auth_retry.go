package server

import (
	"strings"

	sentinel "github.com/router-for-me/CLIProxyAPI/v7/wukong/sentinel"
)

func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "401") ||
		strings.Contains(s, "unauthorized") ||
		strings.Contains(s, "invalid_token") ||
		strings.Contains(s, "token expired")
}

func (e *Engine) chatStreamWithRetry(
	env ChatEnv,
	entry *sessionEntry,
	opts sentinel.ChatOptions,
	handler sentinel.StreamHandler,
) (*sentinel.ChatResult, error) {
	result, err := entry.client.ChatStream(opts, handler)
	if err == nil || !isAuthError(err) || !env.FromPool || e.pool == nil {
		return result, err
	}
	return e.retryAfterRefresh(entry, opts, handler, err)
}

func (e *Engine) chatWithRetry(env ChatEnv, entry *sessionEntry, opts sentinel.ChatOptions) (*sentinel.ChatResult, error) {
	return e.chatStreamWithRetry(env, entry, opts, nil)
}

func (e *Engine) retryAfterRefresh(
	entry *sessionEntry,
	opts sentinel.ChatOptions,
	handler sentinel.StreamHandler,
	firstErr error,
) (*sentinel.ChatResult, error) {
	oldAT := entry.token
	newAT, ok := e.pool.TryRefreshAT(oldAT)
	if !ok {
		e.pool.MarkError(oldAT)
		return nil, firstErr
	}
	entry.client.SetBearerToken(newAT)
	entry.token = newAT
	return entry.client.ChatStream(opts, handler)
}
