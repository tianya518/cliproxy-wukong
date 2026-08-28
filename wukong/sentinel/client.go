package sentinel

import (
	"context"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/imroc/req/v3"
)

// 浏览器身份。UA、sec-ch-ua、full-version-list 三者的版本号必须一致——真实浏览器
// 不可能自称 Chrome 147 又报 sec-ch-ua 146。全部由下面几个常量推导，改版本时只改常量，
// TestBrowserIdentityConsistent 会拦住漂移。取值来自 Edge 146 / Windows 11 实抓。
const (
	chromeMajor     = "146"
	chromiumFullVer = "146.0.7680.154"
	edgeFullVer     = "146.0.3856.72"

	defaultUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) " +
		"Chrome/" + chromeMajor + ".0.0.0 Safari/537.36 Edg/" + chromeMajor + ".0.0.0"

	secCHUA            = `"Chromium";v="` + chromeMajor + `", "Not-A.Brand";v="24", "Microsoft Edge";v="` + chromeMajor + `"`
	secCHUAFullVersion = `"` + edgeFullVer + `"`
	secCHUAVersionList = `"Chromium";v="` + chromiumFullVer + `", "Not-A.Brand";v="24.0.0.0", "Microsoft Edge";v="` + edgeFullVer + `"`

	defaultBuildHash   = "prod-81e0c5cdf6140e8c5db714d613337f4aeab94029"
	defaultBuildNumber = "6128297"
	defaultLang        = "zh-CN"
	defaultModel       = "gpt-5-5-thinking"
)

// ImpersonateChrome 铺的是「地址栏敲回车」那种顶层导航请求的头，
// 而我们发的全是 backend-api 的 XHR，这几个头真实浏览器不会带。
var navigationOnlyHeaders = []string{
	"Upgrade-Insecure-Requests",
	"Sec-Fetch-User",
	"Pragma",
	"Cache-Control",
}

// Client 是 ChatGPT 对话客户端，封装了完整的 Sentinel 认证 + SSE 对话流程。
type Client struct {
	httpClient  *req.Client
	bearerToken string
	cookieStr   string
	userAgent   string
	deviceID    string
	buildHash   string
	buildNumber string
	language    string
	sessionID   string
	imageDir    string
	startTime   time.Time

	conversationID  string
	parentMessageID string
	gizmoID         string
	model           string
	tempMode        bool
	turnCount       int

	// Logf 日志输出函数，设为 nil 可禁用日志。默认 log.Printf。
	Logf LogFunc

	// DisableAutoImage 设为 true 时，Chat/ChatStream 不会自动阻塞等待图片下载。
	// 适合 DLL / 外部调用场景，由调用方自己异步处理图片下载。
	DisableAutoImage bool

	// StreamRecorder 非空时记录全部 SSE 事件（供 stream-capture 分析）。
	StreamRecorder *StreamRecorder

	proxyURL string // 出站代理（HTTP + WebSocket）
}

// NewClient 创建新的 ChatGPT 客户端
func NewClient(cfg Config) *Client {
	c := &Client{
		bearerToken:     cfg.BearerToken,
		cookieStr:       cfg.CookieString,
		userAgent:       orDefault(cfg.UserAgent, defaultUA),
		deviceID:        orDefault(cfg.DeviceID, GenerateUUID()),
		buildHash:       orDefault(cfg.BuildHash, defaultBuildHash),
		buildNumber:     orDefault(cfg.BuildNumber, defaultBuildNumber),
		language:        orDefault(cfg.Language, defaultLang),
		imageDir:        orDefault(cfg.ImageDir, "images"),
		model:           orDefault(cfg.Model, defaultModel),
		parentMessageID: "client-created-root",
		sessionID:       GenerateUUID(),
		startTime:       time.Now(),
		tempMode:        cfg.TempMode,
		Logf:            log.Printf,
	}

	// 顺序要紧：ImpersonateChrome 内部也调 SetCommonHeaders，而 http.Header.Set 会规范化
	// key 的大小写，它的 "user-agent" 和我们的 "User-Agent" 是同一个键。先伪装、后铺自己的头，
	// 否则整套身份会被它的 Chrome 120 / macOS 默认值顶掉。
	httpC := req.C().
		SetBaseURL("https://chatgpt.com").
		ImpersonateChrome().
		SetTLSFingerprintChrome(). // ImpersonateChrome 写死 Chrome 120，换成 utls 里最新的
		SetCommonHeaders(c.commonHeaders())
	for _, k := range navigationOnlyHeaders {
		httpC.Headers.Del(k)
	}
	httpC.SetTLSHandshakeTimeout(20 * time.Second)

	// 默认强制 IPv4 拨号：隧道/代理环境下 chatgpt.com 的 IPv6 路径常出现 TLS 握手卡死，
	// 而纯 Go 的 utls 传输不会像 libcurl 那样在超时内回退到 IPv4。设 SENTINEL_ALLOW_IPV6=1 恢复双栈。
	if os.Getenv("SENTINEL_ALLOW_IPV6") == "" {
		dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
		httpC.SetDial(func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		})
	}

	if proxyURL := strings.TrimSpace(cfg.ProxyURL); proxyURL != "" {
		c.proxyURL = proxyURL
		httpC.SetProxyURL(proxyURL)
		if c.Logf != nil {
			c.Logf("[proxy] 出站代理: %s", proxyURL)
		}
	}

	c.httpClient = httpC
	return c
}

// HTTPClient 返回底层 req.Client 以便高级自定义
func (c *Client) HTTPClient() *req.Client {
	return c.httpClient
}

// ResetSession 重置对话上下文（开始新对话）
func (c *Client) ResetSession() {
	c.conversationID = ""
	c.parentMessageID = "client-created-root"
	c.turnCount = 0
}

// SetModel 切换模型
func (c *Client) SetModel(model string) { c.model = model }

// GetModel 获取当前模型
func (c *Client) GetModel() string { return c.model }

// SetTempMode 设置临时模式
func (c *Client) SetTempMode(enabled bool) { c.tempMode = enabled }

// SetDisableAutoImage 设置是否禁用自动图片下载（DLL 场景使用）
func (c *Client) SetDisableAutoImage(disabled bool) { c.DisableAutoImage = disabled }

// SetBearerToken 更新 Bearer Token（Session Token 刷新后调用）。
func (c *Client) SetBearerToken(token string) {
	c.bearerToken = token
	c.httpClient.SetCommonHeader("Authorization", "Bearer "+token)
}

// SetConversationID 恢复到指定对话
func (c *Client) SetConversationID(id string) { c.conversationID = id }

// SetGizmoID 把后续对话挂到指定项目 / GPT（g-p-… 或 g-…）。空串表示普通聊天。
func (c *Client) SetGizmoID(id string) { c.gizmoID = NormalizeGizmoID(id) }

// GizmoID 当前绑定的项目 / GPT id。
func (c *Client) GizmoID() string { return c.gizmoID }

// SetParentMessageID 设置父消息 ID（用于指定回复位置）
func (c *Client) SetParentMessageID(id string) { c.parentMessageID = id }

// GetSessionInfo 获取当前会话状态
func (c *Client) GetSessionInfo() SessionInfo {
	return SessionInfo{
		ConversationID:  c.conversationID,
		ParentMessageID: c.parentMessageID,
		GizmoID:         c.gizmoID,
		Model:           c.model,
		TempMode:        c.tempMode,
		TurnCount:       c.turnCount,
	}
}

func (c *Client) logf(format string, args ...interface{}) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func (c *Client) commonHeaders() map[string]string {
	h := map[string]string{
		"Authorization":               "Bearer " + c.bearerToken,
		"User-Agent":                  c.userAgent,
		"Accept":                      "*/*",
		"Accept-Language":             c.language + ",zh;q=0.9,en;q=0.8,en-GB;q=0.7,en-US;q=0.6",
		"oai-language":                c.language,
		"oai-device-id":               c.deviceID,
		"oai-session-id":              c.sessionID,
		"oai-client-version":          c.buildHash,
		"oai-client-build-number":     c.buildNumber,
		"Origin":                      "https://chatgpt.com",
		"Referer":                     "https://chatgpt.com/",
		"sec-ch-ua":                   secCHUA,
		"sec-ch-ua-mobile":            "?0",
		"sec-ch-ua-platform":          `"Windows"`,
		"sec-ch-ua-platform-version":  `"19.0.0"`,
		"sec-ch-ua-arch":              `"x86"`,
		"sec-ch-ua-bitness":           `"64"`,
		"sec-ch-ua-model":             `""`,
		"sec-ch-ua-full-version":      secCHUAFullVersion,
		"sec-ch-ua-full-version-list": secCHUAVersionList,
		"sec-fetch-dest":              "empty",
		"sec-fetch-mode":              "cors",
		"sec-fetch-site":              "same-origin",
	}
	if c.cookieStr != "" {
		h["Cookie"] = c.cookieStr
	}
	return h
}
