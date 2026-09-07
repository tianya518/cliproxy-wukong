package server

import (
	"os"
	"strconv"
	"strings"
)

// ServerConfig 服务器配置，全部从环境变量读取
type ServerConfig struct {
	// HTTP 服务
	Port string // 监听端口，默认 5005

	// 鉴权：调用本服务的 API Key（区别于 ChatGPT Bearer Token）
	// 若为空，则不校验 Authorization 头（直接将传入的 token 当作 ChatGPT token 使用）
	Authorization string

	// ChatGPT 客户端默认参数
	DefaultModel string // 默认模型，默认 gpt-5-5
	// TempMode 临时模式（不保存对话历史、不更新账号记忆），默认 false，即普通会话。
	// 设 TEMP_MODE=true 可隔离账号级跨会话记忆——否则 ChatGPT 可能把先前无关请求的
	// 内容带进新会话。开启后生图与项目对话仍会自动豁免（见 engine.go）。
	TempMode bool
	ImageDir string // 图片保存目录，默认 images

	// ChatGPT 网页凭证池（JWT / session / refresh）。默认 chatgpt.json。
	// 旧文件名 tokens.json、旧环境变量 TOKENS_FILE 仍然认。
	ChatGPTFile string
	// Grok Web SSO 账号。默认 grok.json，和 ChatGPT 凭证分开。
	GrokFile string

	// Session 管理
	SessionTTLMinutes int // Session 不活跃超时（分钟），默认 120

	// 对外地址（可选），用于生成绝对资源链接（图片/PDF 代理 URL）
	// 例如：http://192.168.1.10:5005 或 https://your.domain
	// 若为空，则从请求的 Host / X-Forwarded-Proto 头自动推断
	BaseURL string

	// 出站代理（访问 chatgpt.com），如 socks5://127.0.0.1:10816
	ProxyURL string

	// Token 自动刷新：在 AT 过期前多少秒提前用 ST/RT 换 AT，默认 86400（1 天）
	TokenRefreshAheadSec int

	// 后台定时刷新循环间隔（秒），默认 1800（30 分钟）；<=0 关闭后台循环
	RefreshLoopSec int

	// refresh_token 换 AT 的 OAuth 端点与 client_id（留空用默认 auth.openai.com）
	OAuthTokenURL string
	OAuthClientID string

	// 产物存储（见 docs/ARTIFACT_STORE_PLAN.md）：持久映射 + 磁盘缓存。
	ArtifactDir             string // 缓存目录，默认 artifacts
	ArtifactIndexPath       string // 映射表文件，默认 <ArtifactDir>/index.jsonl
	ArtifactPublicPath      string // 对外路径前缀，默认 /files
	ArtifactMaxTotalMB      int    // 缓存总大小上限（MB），默认 2048；<=0 不限
	ArtifactMaxAgeDays      int    // 缓存按天清理，默认 0 不限
	ArtifactFetchTimeoutSec int    // 访问时回源单次超时，默认 60
	ArtifactVideoTimeoutSec int    // Grok 视频后台预下载超时，默认 120
	ArtifactInlineImages    bool   // 生成图是否同时以 images[] 内联 base64 返回，默认 true
	ArtifactInlineMaxMB     int    // 单张内联上限（MB），默认 8；超过只给链接
	ArtifactHistoryReattach int    // 无 conversation_id 时从历史回挂的图片张数上限，默认 4；0 关闭
}

// LoadConfig 从环境变量加载配置
func LoadConfig() ServerConfig {
	return ServerConfig{
		Port:                 getEnv("PORT", "5005"),
		Authorization:        getEnv("AUTHORIZATION", ""),
		DefaultModel:         getEnv("DEFAULT_MODEL", "gpt-5-5-thinking"),
		TempMode:             getEnvBool("TEMP_MODE", false),
		ImageDir:             getEnv("IMAGE_DIR", "images"),
		ChatGPTFile:          resolveChatGPTFile(),
		GrokFile:             getEnv("GROK_FILE", "grok.json"),
		SessionTTLMinutes:    getEnvInt("SESSION_TTL_MINUTES", 120),
		BaseURL:              getEnv("BASE_URL", ""),
		ProxyURL:             getEnv("PROXY_URL", getEnv("ALL_PROXY", "")),
		TokenRefreshAheadSec: getEnvInt("TOKEN_REFRESH_AHEAD_SEC", 86400),
		RefreshLoopSec:       getEnvInt("REFRESH_LOOP_SEC", 1800),
		OAuthTokenURL:        getEnv("OAUTH_TOKEN_URL", ""),
		OAuthClientID:        getEnv("OAUTH_CLIENT_ID", ""),

		ArtifactDir:             getEnv("ARTIFACT_DIR", "artifacts"),
		ArtifactIndexPath:       getEnv("ARTIFACT_INDEX_PATH", ""),
		ArtifactPublicPath:      getEnv("ARTIFACT_PUBLIC_PATH", "/files"),
		ArtifactMaxTotalMB:      getEnvInt("ARTIFACT_MAX_TOTAL_MB", 2048),
		ArtifactMaxAgeDays:      getEnvInt("ARTIFACT_MAX_AGE_DAYS", 0),
		ArtifactFetchTimeoutSec: getEnvInt("ARTIFACT_FETCH_TIMEOUT_SEC", 60),
		ArtifactVideoTimeoutSec: getEnvInt("ARTIFACT_VIDEO_TIMEOUT_SEC", 120),
		ArtifactInlineImages:    getEnvBool("ARTIFACT_INLINE_IMAGES", true),
		ArtifactInlineMaxMB:     getEnvInt("ARTIFACT_INLINE_MAX_MB", 8),
		ArtifactHistoryReattach: getEnvInt("ARTIFACT_HISTORY_REATTACH_MAX", 4),
	}
}

// resolveChatGPTFile 决定 ChatGPT 凭证文件路径。
//
// 优先级：CHATGPT_FILE → 旧名 TOKENS_FILE → 已存在的 chatgpt.json / tokens.json
// → 默认新建 chatgpt.json。这样改名后旧仓库不用立刻搬文件。
func resolveChatGPTFile() string {
	if v := strings.TrimSpace(os.Getenv("CHATGPT_FILE")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("TOKENS_FILE")); v != "" {
		return v
	}
	if found := firstExistingFile("chatgpt.json", "tokens.json"); found != "" {
		return found
	}
	return "chatgpt.json"
}

func firstExistingFile(paths ...string) string {
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func getEnvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
