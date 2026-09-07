package cliproxy

// artifact_fetchers.go —— 产物存储的回源实现：缓存缺失时按映射里的凭证 ID 从凭证池取当前
// 有效凭证，去上游把字节拉回来。两条网页通道各一个。
//
// 不跨账号：ChatGPT 的 files/download 与 Grok 的 assets 都只对创建它的账号开放，换别的号
// 只会得到 403。映射里没有凭证 ID（升级前回填的旧记录）时才会把同 provider 的凭证逐个试一遍。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"

	"github.com/router-for-me/CLIProxyAPI/v7/wukong/grok"
	sentinel "github.com/router-for-me/CLIProxyAPI/v7/wukong/sentinel"
	sentinelserver "github.com/router-for-me/CLIProxyAPI/v7/wukong/server"
)

// ErrArtifactCredentialMissing 映射里的凭证已不在池中。
var ErrArtifactCredentialMissing = errors.New("credential for artifact is no longer available")

// candidateAuths 返回可用于回源的凭证：指定了 AuthID 就只有它；否则同 provider 全部。
func candidateAuths(mgr *coreauth.Manager, provider, authID string) []*coreauth.Auth {
	if mgr == nil {
		return nil
	}
	if authID = strings.TrimSpace(authID); authID != "" {
		if auth, ok := mgr.GetByID(authID); ok && auth != nil && !auth.Disabled {
			return []*coreauth.Auth{auth}
		}
		return nil
	}
	var out []*coreauth.Auth
	for _, auth := range mgr.List() {
		if auth != nil && !auth.Disabled && strings.EqualFold(auth.Provider, provider) {
			out = append(out, auth)
		}
	}
	return out
}

// ChatGPTArtifactFetcher 回源 ChatGPT 网页的生图与沙箱文件。
type ChatGPTArtifactFetcher struct {
	mgr *coreauth.Manager
	cfg *sentinelserver.ServerConfig
}

// NewChatGPTArtifactFetcher 构造；cfg 提供出站代理与图片目录等 sentinel 客户端参数。
func NewChatGPTArtifactFetcher(mgr *coreauth.Manager, cfg *sentinelserver.ServerConfig) *ChatGPTArtifactFetcher {
	return &ChatGPTArtifactFetcher{mgr: mgr, cfg: cfg}
}

var _ sentinelserver.ArtifactFetcher = (*ChatGPTArtifactFetcher)(nil)

func (f *ChatGPTArtifactFetcher) Fetch(ctx context.Context, rec sentinelserver.ArtifactRecord) (io.ReadCloser, string, error) {
	loc := rec.Locator
	if loc.ConvID == "" || (loc.FileID == "" && loc.SandboxPath == "") {
		return nil, "", fmt.Errorf("chatgpt-web artifact %s: incomplete locator", rec.ID)
	}
	auths := candidateAuths(f.mgr, ProviderKey, rec.AuthID)
	if len(auths) == 0 {
		return nil, "", fmt.Errorf("chatgpt-web artifact %s: %w (auth=%q)", rec.ID, ErrArtifactCredentialMissing, rec.AuthID)
	}
	var lastErr error
	for _, auth := range auths {
		token, err := accessTokenFrom(auth)
		if err != nil || token == "" {
			lastErr = err
			continue
		}
		client := sentinel.NewClient(sentinel.Config{
			BearerToken: token,
			ProxyURL:    f.proxyURL(),
			ImageDir:    f.imageDir(),
		})
		var (
			data     []byte
			mimeType string
		)
		if loc.FileID != "" {
			data, mimeType, err = client.DownloadFileByFileID(loc.ConvID, loc.FileID)
		} else {
			data, mimeType, err = client.DownloadSandboxFile(loc.ConvID, loc.MessageID, loc.SandboxPath)
		}
		if err != nil {
			lastErr = err
			continue
		}
		if len(data) == 0 {
			lastErr = errors.New("empty body")
			continue
		}
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return io.NopCloser(bytes.NewReader(data)), mimeType, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no usable credential")
	}
	return nil, "", fmt.Errorf("chatgpt-web artifact %s: %w", rec.ID, lastErr)
}

func (f *ChatGPTArtifactFetcher) proxyURL() string {
	if f.cfg == nil {
		return ""
	}
	return f.cfg.ProxyURL
}

func (f *ChatGPTArtifactFetcher) imageDir() string {
	if f.cfg == nil {
		return ""
	}
	return f.cfg.ImageDir
}

// GrokArtifactFetcher 回源 Grok 网页的图片与视频（assets.grok.com 等受信资源域名）。
type GrokArtifactFetcher struct {
	mgr *coreauth.Manager
	cfg grok.Config
}

// NewGrokArtifactFetcher 构造；cfg 与 GrokExecutor 用同一份（代理、超时、Statsig 等）。
func NewGrokArtifactFetcher(mgr *coreauth.Manager, cfg grok.Config) *GrokArtifactFetcher {
	return &GrokArtifactFetcher{mgr: mgr, cfg: cfg}
}

var _ sentinelserver.ArtifactFetcher = (*GrokArtifactFetcher)(nil)

func (f *GrokArtifactFetcher) Fetch(ctx context.Context, rec sentinelserver.ArtifactRecord) (io.ReadCloser, string, error) {
	if strings.TrimSpace(rec.Locator.URL) == "" {
		return nil, "", fmt.Errorf("grok-web artifact %s: missing url", rec.ID)
	}
	auths := candidateAuths(f.mgr, GrokProviderKey, rec.AuthID)
	if len(auths) == 0 {
		return nil, "", fmt.Errorf("grok-web artifact %s: %w (auth=%q)", rec.ID, ErrArtifactCredentialMissing, rec.AuthID)
	}
	var lastErr error
	for _, auth := range auths {
		cred, err := grokCredentialFrom(auth)
		if err != nil {
			lastErr = err
			continue
		}
		body, mimeType, err := grok.NewClient(f.cfg, cred).DownloadAsset(ctx, rec.Locator.URL)
		if err != nil {
			lastErr = err
			continue
		}
		return body, mimeType, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no usable credential")
	}
	return nil, "", fmt.Errorf("grok-web artifact %s: %w", rec.ID, lastErr)
}
