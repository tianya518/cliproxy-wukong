package cliproxy

// auth.go —— 从 cliproxy 的 Auth 记录里取出 ChatGPT access token，
// 以及把 wukong 的 chatgpt.json 灌进 cliproxy 的凭证池。

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"

	sentinelserver "github.com/router-for-me/CLIProxyAPI/v7/wukong/server"
)

// 凭证在 Auth 记录里可能出现的位置，按优先级排列。
// Attributes 用于配置写死的静态密钥，Metadata 用于 auths/*.json 里的可变状态。
var (
	attributeKeys = []string{"api_key", "access_token", "accessToken"}
	metadataKeys  = []string{"access_token", "accessToken", "api_key"}
)

// accessTokenFrom 从 Auth 记录中解析出可用的 ChatGPT access token。
func accessTokenFrom(auth *coreauth.Auth) (string, error) {
	if auth == nil {
		return "", requestError{fmt.Errorf("%s: no credential selected", ProviderKey)}
	}
	for _, k := range attributeKeys {
		if v := strings.TrimSpace(auth.Attributes[k]); v != "" {
			return normalizeCredential(v)
		}
	}
	for _, k := range metadataKeys {
		if s, ok := auth.Metadata[k].(string); ok && strings.TrimSpace(s) != "" {
			return normalizeCredential(s)
		}
	}
	return "", fmt.Errorf("%s: auth %q carries no access token", ProviderKey, auth.ID)
}

// normalizeCredential 兼容 wukong 支持的各种凭证写法（session JSON、
// <access>----<session>、rt:<refresh>、裸 token）。
func normalizeCredential(raw string) (string, error) {
	cred, ok := sentinelserver.ParseCredential(raw)
	if !ok || cred.AccessToken == "" {
		return "", fmt.Errorf("%s: credential has no usable access token", ProviderKey)
	}
	return cred.AccessToken, nil
}

// chatgptFile 对应 wukong 的 chatgpt.json 结构（旧名 tokens.json 同形）。
type chatgptFile struct {
	Tokens []struct {
		ID          string `json:"id"`
		AccessToken string `json:"access_token"`
	} `json:"tokens"`
}

// RegisterAuthsFromChatGPTFile 把 ChatGPT 网页凭证注册成 cliproxy 的 chatgpt-web。
//
// 这样 ChatGPT 账号就进入了 cliproxy 的凭证池，能享受轮换、冷却、失败重试，
// 而不必在 wukong 侧再维护一套调度。文件不存在时返回 0 而非报错，
// 允许先起服务再通过管理接口灌号。
func RegisterAuthsFromChatGPTFile(ctx context.Context, mgr *coreauth.Manager, path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read chatgpt file: %w", err)
	}
	var tf chatgptFile
	if err = json.Unmarshal(raw, &tf); err != nil {
		return 0, fmt.Errorf("parse chatgpt file: %w", err)
	}

	n := 0
	now := time.Now().UTC()
	for _, t := range tf.Tokens {
		token := strings.TrimSpace(t.AccessToken)
		if token == "" {
			continue
		}
		id := t.ID
		if id == "" {
			sum := sha256.Sum256([]byte(token))
			id = fmt.Sprintf("%x", sum[:6])
		}
		auth := &coreauth.Auth{
			ID:         ProviderKey + "-" + id,
			Provider:   ProviderKey,
			Label:      ProviderKey + ":" + id,
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"api_key": token},
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if _, err = mgr.Register(ctx, auth); err != nil {
			return n, fmt.Errorf("register auth %s: %w", auth.ID, err)
		}
		n++
	}
	return n, nil
}

// RegisterAuthsFromTokensFile 是 RegisterAuthsFromChatGPTFile 的旧名。
func RegisterAuthsFromTokensFile(ctx context.Context, mgr *coreauth.Manager, path string) (int, error) {
	return RegisterAuthsFromChatGPTFile(ctx, mgr, path)
}
