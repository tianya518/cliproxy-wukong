package cliproxy

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

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

	// 产物存储（可为空）。挂上后 assets.grok.com 直链不再透传：登记映射 → 下载进缓存 → 换成 /files/<id>。
	store        *sentinelserver.ArtifactStore
	absoluteURL  func(string) string
	videoTimeout time.Duration
}

func NewGrokExecutor(cfg grok.Config) *GrokExecutor {
	return &GrokExecutor{cfg: cfg}
}

// SetArtifactStore 挂上产物存储。absoluteURL 把 /files/<id> 拼成对外可达的绝对地址；
// videoTimeout 是视频后台预下载的超时（超时只影响预热，访问时仍会按映射回源）。
func (x *GrokExecutor) SetArtifactStore(store *sentinelserver.ArtifactStore, absoluteURL func(string) string, videoTimeout time.Duration) {
	if x == nil {
		return
	}
	x.store = store
	x.absoluteURL = absoluteURL
	if videoTimeout <= 0 {
		videoTimeout = 120 * time.Second
	}
	x.videoTimeout = videoTimeout
	// 已缓存的 client 也要装上钩子。
	x.clients.Range(func(key, value any) bool {
		if client, ok := value.(*grok.Client); ok {
			client.SetAssetRewriter(x.assetRewriter(key.(string), client))
		}
		return true
	})
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
	if x.store != nil {
		client.SetAssetRewriter(x.assetRewriter(auth.ID, client))
	}
	actual, _ := x.clients.LoadOrStore(auth.ID, client)
	return actual.(*grok.Client), nil
}

// assetRewriter 返回绑定到某凭证 / client 的 URL 改写钩子：
//  1. 登记映射（provider=grok-web、凭证 ID、原始 URL）；
//  2. 图片同步下载进缓存（内联 base64 也靠这份字节），视频后台预下载；
//  3. 返回 /files/<id>.<ext>。任何失败都不回退到原链——映射已在，访问时回源。
func (x *GrokExecutor) assetRewriter(authID string, client *grok.Client) func(ctx context.Context, kind, rawURL string) string {
	return func(ctx context.Context, kind, rawURL string) string {
		rawURL = strings.TrimSpace(rawURL)
		if x.store == nil || rawURL == "" {
			return rawURL
		}
		id := sentinelserver.GrokAssetArtifactID(rawURL)
		ext := sentinelserver.ArtifactExtForName(rawURL)
		recKind := sentinelserver.ArtifactKindImage
		if kind == grok.AssetKindVideo {
			recKind = sentinelserver.ArtifactKindVideo
			if ext == "" {
				ext = "mp4"
			}
		} else if ext == "" {
			ext = "jpg"
		}
		rec := sentinelserver.ArtifactRecord{
			ID: id, Ext: ext, Kind: recKind,
			Provider: sentinelserver.ArtifactProviderGrok, AuthID: authID,
			Locator: sentinelserver.ArtifactLocator{URL: rawURL},
		}
		if err := x.store.Record(rec); err != nil {
			log.Printf("[artifact] 登记 Grok 资源映射失败，原链照发: %v", err)
			return rawURL
		}
		if stored, ok := x.store.Lookup(id); ok {
			rec = stored
		}
		if !x.store.Cached(id) {
			if kind == grok.AssetKindVideo {
				go x.prefetchAsset(context.Background(), client, id, rawURL, x.videoTimeout)
			} else {
				x.prefetchAsset(ctx, client, id, rawURL, 60*time.Second)
			}
		}
		return x.store.PublicURL(x.absoluteURL, rec)
	}
}

func (x *GrokExecutor) prefetchAsset(ctx context.Context, client *grok.Client, id, rawURL string, timeout time.Duration) {
	fctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, mimeType, err := client.DownloadAsset(fctx, rawURL)
	if err != nil {
		log.Printf("[artifact] Grok 资源 %s 预下载失败（访问时回源）: %v", id, err)
		return
	}
	defer body.Close()
	if err := x.store.Put(id, body, mimeType); err != nil {
		log.Printf("[artifact] Grok 资源 %s 写缓存失败: %v", id, err)
	}
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
