package cliproxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	clipexec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktr "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"

	sdkcliproxy "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"

	"github.com/router-for-me/CLIProxyAPI/v7/wukong/grok"
	sentinelserver "github.com/router-for-me/CLIProxyAPI/v7/wukong/server"
)

const GrokProviderKey = "grok-web"

type GrokExecutor struct {
	cfg     grok.Config
	clients sync.Map
}

func NewGrokExecutor(cfg grok.Config) *GrokExecutor {
	return &GrokExecutor{cfg: cfg}
}

func (x *GrokExecutor) Identifier() string { return GrokProviderKey }

func (x *GrokExecutor) RequestToFormat(clipexec.Request, clipexec.Options) sdktr.Format {
	return sdktr.FormatOpenAI
}

func (x *GrokExecutor) clientFor(auth *coreauth.Auth) (*grok.Client, error) {
	cred, err := grokCredentialFrom(auth)
	if err != nil {
		return nil, err
	}
	if existing, ok := x.clients.Load(auth.ID); ok {
		return existing.(*grok.Client), nil
	}
	client := grok.NewClient(x.cfg, cred)
	actual, _ := x.clients.LoadOrStore(auth.ID, client)
	return actual.(*grok.Client), nil
}

func toGrokRequest(req sentinelserver.ChatCompletionRequest) grok.ChatRequest {
	messages := make([]grok.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		messages = append(messages, grok.Message{Role: m.Role, Content: m.Content})
	}
	return grok.ChatRequest{
		Model:          req.Model,
		Messages:       messages,
		Stream:         req.Stream,
		ConversationID: req.ConversationID,
		Size:           req.Size,
		N:              req.N,
	}
}

func (x *GrokExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req clipexec.Request, opts clipexec.Options) (clipexec.Response, error) {
	chatReq, translatedReq, err := decodeChatRequest(GrokProviderKey, req, opts)
	if err != nil {
		return clipexec.Response{}, err
	}
	client, err := x.clientFor(auth)
	if err != nil {
		return clipexec.Response{}, err
	}
	result, err := client.Complete(ctx, toGrokRequest(chatReq))
	if err != nil {
		return clipexec.Response{}, classify(err)
	}
	payload, err := json.Marshal(grok.OpenAICompletion(result))
	if err != nil {
		return clipexec.Response{}, err
	}
	var param any
	out := sdktr.TranslateNonStream(ctx, sdktr.FormatOpenAI, clipexec.ResponseFormatOrSource(opts),
		req.Model, opts.OriginalRequest, translatedReq, payload, &param)
	return clipexec.Response{
		Payload: out,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

func (x *GrokExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req clipexec.Request, opts clipexec.Options) (*clipexec.StreamResult, error) {
	chatReq, translatedReq, err := decodeChatRequest(GrokProviderKey, req, opts)
	if err != nil {
		return nil, err
	}
	client, err := x.clientFor(auth)
	if err != nil {
		return nil, err
	}
	chatReq.Stream = true
	responseFormat := clipexec.ResponseFormatOrSource(opts)
	ch := make(chan clipexec.StreamChunk, streamChunkBuffer)
	go func() {
		defer close(ch)
		send := func(c clipexec.StreamChunk) bool {
			select {
			case ch <- c:
				return true
			case <-ctx.Done():
				return false
			}
		}
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
		id := "chatcmpl-grok"
		first := true
		result, streamErr := client.Stream(ctx, toGrokRequest(chatReq), func(delta grok.StreamDelta) {
			if first {
				first = false
				payload, _ := json.Marshal(grok.OpenAIChunk(id, req.Model, "assistant", "", nil, ""))
				forward(append([]byte("data: "), payload...))
			}
			text := delta.Text
			if delta.Kind == "image" && delta.Image != "" {
				text = "![Generated Image](" + delta.Image + ")"
			}
			if text == "" {
				return
			}
			payload, _ := json.Marshal(grok.OpenAIChunk(id, req.Model, "", text, nil, ""))
			forward(append([]byte("data: "), payload...))
		})
		if streamErr != nil {
			send(clipexec.StreamChunk{Err: classify(streamErr)})
			return
		}
		if result != nil && first {
			payload, _ := json.Marshal(grok.OpenAIChunk(id, req.Model, "assistant", result.Text, nil, result.ConversationID))
			forward(append([]byte("data: "), payload...))
		}
		stop := "stop"
		if result != nil && result.FinishReason != "" {
			stop = result.FinishReason
		}
		payload, _ := json.Marshal(grok.OpenAIChunk(id, req.Model, "", "", &stop, ""))
		forward(append([]byte("data: "), payload...))
		forward([]byte("data: [DONE]"))
	}()
	return &clipexec.StreamResult{
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Chunks:  ch,
	}, nil
}

func (x *GrokExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (x *GrokExecutor) CountTokens(context.Context, *coreauth.Auth, clipexec.Request, clipexec.Options) (clipexec.Response, error) {
	return clipexec.Response{}, errors.New(GrokProviderKey + ": count tokens not supported")
}

func (x *GrokExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New(GrokProviderKey + ": raw http passthrough not supported")
}

func grokModelIDs() []string {
	return grok.PublicModelIDs()
}

func grokModelInfos(ids []string) []*sdkcliproxy.ModelInfo {
	out := make([]*sdkcliproxy.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, &sdkcliproxy.ModelInfo{
			ID: id, Object: "model", Type: GrokProviderKey, DisplayName: id, OwnedBy: "xai",
		})
	}
	return out
}
