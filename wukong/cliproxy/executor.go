// Package cliproxy 把 wukong 的 ChatGPT 网页逆向接成 CLIProxyAPI 的 provider。
//
// 集成方式是「把 cliproxy 当库用」，而不是 fork 它：本包只在 go.mod 里 require
// CLIProxyAPI，不修改它任何一行代码，因此上游升级就是 go get -u，永远不会有
// 合并冲突。这一点很重要——CLIProxyAPI 每月约 290 个提交，而新增内置 provider
// 需要改动 9 处中央文件（provider 列表、config schema 等），正是冲突高发区。
//
// 协议翻译不用自己写：RequestToFormat 声明本 executor 收 OpenAI chat-completions
// 格式，cliproxy 就会用内置 translator 把 Claude / Gemini 入站请求翻译好再送进来，
// 响应方向同理翻回去。
package cliproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	clipexec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktr "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"

	sentinelserver "github.com/router-for-me/CLIProxyAPI/v7/wukong/server"
)

// ProviderKey 是本 provider 在 cliproxy 中的标识。
//
// 不能叫 "codex"——那是 cliproxy 内置的 OAuth Codex provider，同名会被内置实现顶掉。
const ProviderKey = "chatgpt-web"

// streamChunkBuffer 流式 chunk 的发送缓冲，给下游消费留一点余量。
const streamChunkBuffer = 16

// Executor 把 cliproxy 的执行请求转成 wukong 的一轮对话。
type Executor struct {
	engine *sentinelserver.Engine
	// artifactBaseURL 生图/沙箱产物的对外基地址。cliproxy 形态下产物仍由
	// wukong 自己的 HTTP 路由提供，这里指向那个地址，否则末端客户端拿到的
	// 是取不到的相对路径。
	artifactBaseURL string
	oauthTokenURL   string
	oauthClientID   string

	refreshFromRefreshToken func(refreshToken, oauthURL, clientID string) (accessToken, newRefreshToken string, expiresAt time.Time, err error)
	refreshFromSession      func(sessionToken string) (accessToken string, expiresAt time.Time, err error)
}

// SetOAuthConfig overrides the auth.openai.com endpoint used by Refresh.
func (x *Executor) SetOAuthConfig(oauthURL, clientID string) {
	if x == nil {
		return
	}
	x.oauthTokenURL = strings.TrimSpace(oauthURL)
	x.oauthClientID = strings.TrimSpace(clientID)
}

// NewExecutor 创建 executor。engine 与产物路由必须共用同一个 SessionManager，
// 否则按 conversation_id 反查会话会失败，图片代理取不到图。
func NewExecutor(engine *sentinelserver.Engine, artifactBaseURL string) *Executor {
	return &Executor{
		engine:          engine,
		artifactBaseURL: strings.TrimRight(strings.TrimSpace(artifactBaseURL), "/"),
	}
}

// Identifier 返回 provider 名。
func (x *Executor) Identifier() string { return ProviderKey }

// RequestToFormat 声明入站请求要先翻译成 OpenAI chat-completions 再交给本 executor。
// 有了它，Claude / Gemini 客户端由 cliproxy 内置 translator 覆盖，本包不需要写翻译。
func (x *Executor) RequestToFormat(clipexec.Request, clipexec.Options) sdktr.Format {
	return sdktr.FormatOpenAI
}

// env 组装本轮请求的环境依赖。
func (x *Executor) env(ctx context.Context, auth *coreauth.Auth) (sentinelserver.ChatEnv, error) {
	token, err := accessTokenFrom(auth)
	if err != nil {
		return sentinelserver.ChatEnv{}, err
	}
	return sentinelserver.ChatEnv{
		Ctx:   ctx,
		Token: token,
		// 凭证轮换与失败重试交给 cliproxy 的凭证池，这里不要再自己换票，
		// 否则两层重试叠加会放大一次故障的请求量。
		FromPool:    false,
		AbsoluteURL: x.absoluteURL,
	}, nil
}

func (x *Executor) absoluteURL(path string) string {
	if x.artifactBaseURL == "" ||
		strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	return x.artifactBaseURL + path
}

// decodeRequest 把入站负载翻译成 OpenAI chat-completions 并解析成本地请求，
// 同时返回翻译后的负载（响应方向的翻译器需要它作为上下文）。
//
// 请求翻译必须由 executor 自己做：conductor 不代劳，RequestToFormat 只是一个
// 声明，供请求拦截器和路由参考。不翻的话 Gemini 的 {"contents":[...]} 会原样
// 送进来，解析出零条消息；Claude 更隐蔽——它的 messages 结构与 OpenAI 部分重
// 合，纯文本能歪打正着跑通，一旦带 system 或多模态就会悄悄丢内容。
func decodeRequest(req clipexec.Request, opts clipexec.Options) (sentinelserver.ChatCompletionRequest, []byte, error) {
	return decodeChatRequest(ProviderKey, req, opts)
}

func decodeChatRequest(provider string, req clipexec.Request, opts clipexec.Options) (sentinelserver.ChatCompletionRequest, []byte, error) {
	var out sentinelserver.ChatCompletionRequest

	payload := req.Payload
	from := opts.SourceFormat
	if from != "" && from != sdktr.FormatOpenAI {
		payload = sdktr.TranslateRequest(from, sdktr.FormatOpenAI, req.Model, payload, opts.Stream)
	}

	if err := json.Unmarshal(payload, &out); err != nil {
		return out, payload, requestError{fmt.Errorf("%s: decode chat-completions payload: %w", provider, err)}
	}
	// 解析成功但一条 message 都没有，多半是翻译没生效而非客户端发了空请求。
	// 把来源格式和实际形状带出来，否则只会看到一句「没有用户消息」，无从判断。
	if len(out.Messages) == 0 {
		return out, payload, requestError{fmt.Errorf(
			"%s: payload has no messages (source=%q): %s",
			provider, from, truncate(payload, 220))}
	}
	// req.Model 是别名解析后的上游模型名，优先于负载里客户端原始写的那个。
	if strings.TrimSpace(req.Model) != "" {
		out.Model = req.Model
	}
	return out, payload, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// Execute 处理非流式请求。
func (x *Executor) Execute(ctx context.Context, auth *coreauth.Auth, req clipexec.Request, opts clipexec.Options) (clipexec.Response, error) {
	chatReq, translatedReq, err := decodeRequest(req, opts)
	if err != nil {
		return clipexec.Response{}, err
	}
	env, err := x.env(ctx, auth)
	if err != nil {
		return clipexec.Response{}, err
	}

	resp, err := x.engine.Complete(env, chatReq)
	if err != nil {
		return clipexec.Response{}, classify(err)
	}
	payload, err := json.Marshal(resp)
	if err != nil {
		return clipexec.Response{}, err
	}

	// 响应方向同样由 executor 负责。不翻的话 Claude 客户端会原样收到
	// OpenAI 的 chat.completion JSON，解析不了。
	var param any
	out := sdktr.TranslateNonStream(ctx, sdktr.FormatOpenAI, clipexec.ResponseFormatOrSource(opts),
		req.Model, opts.OriginalRequest, translatedReq, payload, &param)

	return clipexec.Response{
		Payload: out,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// ExecuteStream 处理流式请求，输出 OpenAI 风格的 SSE 帧。
func (x *Executor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req clipexec.Request, opts clipexec.Options) (*clipexec.StreamResult, error) {
	chatReq, translatedReq, err := decodeRequest(req, opts)
	if err != nil {
		return nil, err
	}
	env, err := x.env(ctx, auth)
	if err != nil {
		return nil, err
	}
	chatReq.Stream = true

	responseFormat := clipexec.ResponseFormatOrSource(opts)
	ch := make(chan clipexec.StreamChunk, streamChunkBuffer)

	go func() {
		defer close(ch)

		// 下游断开后必须停止投递，否则 Engine 会一直阻塞在 emit 上把 goroutine 泄掉。
		send := func(c clipexec.StreamChunk) bool {
			select {
			case ch <- c:
				return true
			case <-ctx.Done():
				return false
			}
		}

		// param 必须跨整个流复用：Claude / Gemini 的流式翻译是有状态的
		// （要跟踪 message_start、块序号等），每帧新建会产生不合法的事件序列。
		var param any
		forward := func(sseLine []byte) bool {
			for _, frame := range sdktr.TranslateStream(ctx, sdktr.FormatOpenAI, responseFormat,
				req.Model, opts.OriginalRequest, translatedReq, sseLine, &param) {
				if !send(clipexec.StreamChunk{Payload: frame}) {
					return false
				}
			}
			return true
		}

		emit := func(chunk sentinelserver.ChatCompletionChunk) {
			b, marshalErr := json.Marshal(chunk)
			if marshalErr != nil {
				return
			}
			forward(append([]byte("data: "), b...))
		}

		if streamErr := x.engine.Stream(env, chatReq, emit); streamErr != nil {
			send(clipexec.StreamChunk{Err: classify(streamErr)})
			return
		}
		forward([]byte("data: [DONE]"))
	}()

	return &clipexec.StreamResult{
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Chunks:  ch,
	}, nil
}

// Refresh renews the ChatGPT access token through cliproxy's conductor.
// Prefer OAuth refresh_token; fall back to the web session token.
func (x *Executor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if auth == nil {
		return nil, fmt.Errorf("%s: auth is nil", ProviderKey)
	}
	rt := metadataString(auth, "refresh_token", "refreshToken")
	st := metadataString(auth, "session_token", "sessionToken")
	if rt == "" && st == "" {
		return auth, nil
	}

	refreshRT := x.refreshFromRefreshToken
	if refreshRT == nil {
		refreshRT = sentinelserver.RefreshATFromRefreshToken
	}
	refreshST := x.refreshFromSession
	if refreshST == nil {
		refreshST = sentinelserver.RefreshATFromSession
	}

	var (
		at, newRT string
		exp       time.Time
		err       error
	)
	if rt != "" {
		at, newRT, exp, err = refreshRT(rt, x.oauthTokenURL, x.oauthClientID)
	} else {
		at, exp, err = refreshST(st)
	}
	if err != nil {
		if current, atErr := accessTokenFrom(auth); atErr == nil && current != "" {
			if _, fresh := sentinelserver.AccessTokenFresh(current, time.Now()); fresh {
				return auth, nil
			}
		}
		return nil, err
	}
	applyChatGPTTokens(auth, at, newRT, st, exp)
	return auth, nil
}

func applyChatGPTTokens(auth *coreauth.Auth, accessToken, newRefreshToken, sessionToken string, expiresAt time.Time) {
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["type"] = ProviderKey
	if accessToken != "" {
		auth.Metadata["access_token"] = accessToken
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string)
		}
		auth.Attributes["api_key"] = accessToken
	}
	if newRefreshToken != "" {
		auth.Metadata["refresh_token"] = newRefreshToken
	}
	if sessionToken != "" {
		auth.Metadata["session_token"] = sessionToken
	}
	if !expiresAt.IsZero() {
		auth.Metadata["expired"] = expiresAt.UTC().Format(time.RFC3339)
	}
	auth.Metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
}

// CountTokens 网页端不暴露 token 计数接口。
func (x *Executor) CountTokens(context.Context, *coreauth.Auth, clipexec.Request, clipexec.Options) (clipexec.Response, error) {
	return clipexec.Response{}, errors.New(ProviderKey + ": count tokens not supported")
}

// HttpRequest 本 provider 不支持裸 HTTP 透传（上游需要 sentinel token 与 PoW，
// 不是简单加个 Authorization 头就能直连的）。
func (x *Executor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New(ProviderKey + ": raw http passthrough not supported")
}
