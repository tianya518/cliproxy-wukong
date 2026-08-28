package cliproxy

import (
	"errors"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	clipexec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"

	"github.com/router-for-me/CLIProxyAPI/v7/wukong/grok"
	sentinel "github.com/router-for-me/CLIProxyAPI/v7/wukong/sentinel"
	sentinelserver "github.com/router-for-me/CLIProxyAPI/v7/wukong/server"
)

// ChatGPT 的 access token 必须是 JWT（eyJ 开头、至少两个点），
// 见 server.isAccessToken；随便一个字符串会被凭证解析拒绝。
const (
	jwtA = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJhIn0.sigA"
	jwtB = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJiIn0.sigB"
	jwtC = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJjIn0.sigC"
)

func TestAccessTokenFrom(t *testing.T) {
	cases := []struct {
		name string
		auth *coreauth.Auth
		want string
		ok   bool
	}{
		{"nil auth", nil, "", false},
		{
			"api_key attribute",
			&coreauth.Auth{Attributes: map[string]string{"api_key": jwtA}},
			jwtA, true,
		},
		{
			"access_token attribute",
			&coreauth.Auth{Attributes: map[string]string{"access_token": jwtB}},
			jwtB, true,
		},
		{
			"metadata fallback",
			&coreauth.Auth{Metadata: map[string]any{"accessToken": jwtC}},
			jwtC, true,
		},
		{
			"attribute wins over metadata",
			&coreauth.Auth{
				Attributes: map[string]string{"api_key": jwtA},
				Metadata:   map[string]any{"access_token": jwtB},
			},
			jwtA, true,
		},
		{"no credential", &coreauth.Auth{ID: "empty"}, "", false},
		// 非 JWT 值要尽早失败并给出明确原因，否则只会在上游收到一个含糊的 401。
		{
			"non-JWT value rejected",
			&coreauth.Auth{Attributes: map[string]string{"api_key": "sk-not-a-jwt"}},
			"", false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := accessTokenFrom(tc.auth)
			if tc.ok && err != nil {
				t.Fatalf("accessTokenFrom() 意外报错: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("accessTokenFrom() 应报错，实得 nil")
			}
			if got != tc.want {
				t.Errorf("accessTokenFrom() = %q, 期望 %q", got, tc.want)
			}
		})
	}
}

// TestAccessTokenFromSessionJSON 凭证可能是整段 session JSON，要能抽出 accessToken。
func TestAccessTokenFromSessionJSON(t *testing.T) {
	raw := `{"user":{"id":"u"},"accessToken":"eyJhbGciOi.session.token","expires":"2026-11-22T03:08:46.003Z"}`
	got, err := accessTokenFrom(&coreauth.Auth{Attributes: map[string]string{"api_key": raw}})
	if err != nil {
		t.Fatalf("解析 session JSON 失败: %v", err)
	}
	if got != "eyJhbGciOi.session.token" {
		t.Errorf("accessTokenFrom() = %q, 期望从 session JSON 中取出 accessToken", got)
	}
}

// TestMissingCredentialIsRequestScoped 没选到凭证属于请求问题，不应连累凭证池。
func TestMissingCredentialIsRequestScoped(t *testing.T) {
	_, err := accessTokenFrom(nil)
	if err == nil {
		t.Fatal("期望报错")
	}
	var scoped clipexec.RequestScopedError
	if !errors.As(err, &scoped) || !scoped.IsRequestScoped() {
		t.Errorf("错误类型 %T 应实现 RequestScopedError，否则会给凭证记冷却", err)
	}
}

func TestClassify(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		if classify(nil) != nil {
			t.Error("classify(nil) 应返回 nil")
		}
	})

	t.Run("缺少输入按请求问题上报", func(t *testing.T) {
		err := classify(sentinelserver.ErrNoInput)
		var scoped clipexec.RequestScopedError
		if !errors.As(err, &scoped) || !scoped.IsRequestScoped() {
			t.Errorf("ErrNoInput 应归为 request-scoped，实得 %T", err)
		}
	})

	t.Run("401 带出状态码", func(t *testing.T) {
		err := classify(errors.New("upstream returned 401 unauthorized"))
		var se clipexec.StatusError
		if !errors.As(err, &se) {
			t.Fatalf("应实现 StatusError，实得 %T", err)
		}
		if se.StatusCode() != 401 {
			t.Errorf("StatusCode() = %d, 期望 401（触发换票重试）", se.StatusCode())
		}
	})

	t.Run("429 带出状态码", func(t *testing.T) {
		err := classify(errors.New("rate limit exceeded"))
		var se clipexec.StatusError
		if !errors.As(err, &se) || se.StatusCode() != 429 {
			t.Errorf("限流应映射为 429，实得 %T", err)
		}
	})

	t.Run("Grok 额度与反爬按状态码上报", func(t *testing.T) {
		err := classify(grok.ErrUsageLimit)
		var se clipexec.StatusError
		if !errors.As(err, &se) || se.StatusCode() != 429 {
			t.Errorf("usage limit 应映射 429，实得 %T %v", err, err)
		}
		err = classify(grok.ErrUnauthorized)
		if !errors.As(err, &se) || se.StatusCode() != 401 {
			t.Errorf("unauthorized 应映射 401，实得 %T %v", err, err)
		}
	})

	t.Run("普通错误原样透传", func(t *testing.T) {
		orig := errors.New("connection reset")
		if got := classify(orig); got != orig {
			t.Errorf("无法识别的错误应原样返回，实得 %v", got)
		}
	})

	t.Run("Grok incomplete 不冷却凭证", func(t *testing.T) {
		err := classify(&grok.GatewayStatusError{Status: "incomplete"})
		var scoped clipexec.RequestScopedError
		if !errors.As(err, &scoped) || !scoped.IsRequestScoped() {
			t.Errorf("incomplete 应归为 request-scoped，实得 %T %v", err, err)
		}
	})
}

func TestAbsoluteURL(t *testing.T) {
	x := NewExecutor(nil, "http://127.0.0.1:8318/")
	if got := x.absoluteURL("/api/image/proxy?id=1"); got != "http://127.0.0.1:8318/api/image/proxy?id=1" {
		t.Errorf("absoluteURL() = %q, 基地址末尾斜杠应被去掉", got)
	}
	if got := x.absoluteURL("https://cdn.example/x.png"); got != "https://cdn.example/x.png" {
		t.Errorf("已是绝对地址时不应再拼接，实得 %q", got)
	}

	bare := NewExecutor(nil, "")
	if got := bare.absoluteURL("/api/image/proxy"); got != "/api/image/proxy" {
		t.Errorf("未配置基地址时应保持相对路径，实得 %q", got)
	}
}

func TestDecodeRequestRejectsUntranslatedPayload(t *testing.T) {
	// Gemini 的形状：翻译没生效时会原样送进来，应报出来源格式而不是含糊的「没有消息」。
	req := clipexec.Request{Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)}
	_, _, err := decodeRequest(req, clipexec.Options{})
	if err == nil {
		t.Fatal("零条消息的负载应报错")
	}
	if !strings.Contains(err.Error(), "no messages") {
		t.Errorf("错误信息应说明没有消息，实得 %v", err)
	}
	var scoped clipexec.RequestScopedError
	if !errors.As(err, &scoped) {
		t.Errorf("负载问题应归为 request-scoped，实得 %T", err)
	}
}

func TestDecodeRequestPrefersResolvedModel(t *testing.T) {
	req := clipexec.Request{
		Model:   "gpt-5-6-thinking",
		Payload: []byte(`{"model":"client-alias","messages":[{"role":"user","content":"hi"}]}`),
	}
	got, _, err := decodeRequest(req, clipexec.Options{})
	if err != nil {
		t.Fatalf("decodeRequest 报错: %v", err)
	}
	if got.Model != "gpt-5-6-thinking" {
		t.Errorf("Model = %q, 应采用别名解析后的上游模型名", got.Model)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("消息数 = %d, 期望 1", len(got.Messages))
	}
}

func TestCatalogModelIDsFallback(t *testing.T) {
	prev := sentinel.CurrentModelCatalog()
	sentinel.SetModelCatalog(nil)
	t.Cleanup(func() { sentinel.SetModelCatalog(prev) })

	got := catalogModelIDs()
	if len(got) != len(fallbackModels) {
		t.Fatalf("无目录时应返回兜底列表，实得 %v", got)
	}
}

func TestCatalogModelIDsExpandsEfforts(t *testing.T) {
	cat := &sentinel.ModelCatalog{Models: []sentinel.CatalogModel{
		{Slug: "gpt-5-6-instant"},
		{
			Slug: "gpt-5-6-thinking", ConfigurableThinkingEffort: true,
			ThinkingEfforts: []string{"standard", "extended"},
		},
		// Deep Research 走异步任务流程，不能当普通 chat 模型暴露
		{Slug: "research"},
		// 官网「工作」标签页模型，额度与产品面都和聊天分开
		{Slug: "gpt-5.6-sol-wm", IsWorkModeModel: true},
	}}
	prev := sentinel.CurrentModelCatalog()
	sentinel.SetModelCatalog(cat)
	t.Cleanup(func() { sentinel.SetModelCatalog(prev) })

	got := catalogModelIDs()
	joined := strings.Join(got, ",")
	for _, want := range []string{
		"gpt-5-6-instant",
		"gpt-5-6-thinking",
		"gpt-5-6-thinking-standard",
		"gpt-5-6-thinking-extended",
		sentinel.ModelDALLE3,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("模型列表缺少 %q：%v", want, got)
		}
	}
	for _, id := range got {
		if id == "research" || strings.Contains(id, "-wm") {
			t.Errorf("%q 不应出现在对外聊天模型列表中", id)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate([]byte("short"), 10); got != "short" {
		t.Errorf("truncate() = %q, 未超长时应原样返回", got)
	}
	if got := truncate([]byte("0123456789abc"), 10); got != "0123456789..." {
		t.Errorf("truncate() = %q", got)
	}
}

// TestExecutorSatisfiesInterfaces 锁定与 cliproxy 的契约：接口一旦变化这里先失败。
func TestExecutorSatisfiesInterfaces(t *testing.T) {
	var x any = NewExecutor(nil, "")
	if _, ok := x.(coreauth.ProviderExecutor); !ok {
		t.Error("Executor 未满足 coreauth.ProviderExecutor")
	}
	type resolver interface {
		RequestToFormat(clipexec.Request, clipexec.Options) interface{ String() string }
	}
	_ = resolver(nil)

	exec := NewExecutor(nil, "")
	if got := exec.RequestToFormat(clipexec.Request{}, clipexec.Options{}); got.String() != "openai" {
		t.Errorf("RequestToFormat() = %q, 期望 openai（借此复用内置 Claude/Gemini 翻译）", got)
	}
	if exec.Identifier() != ProviderKey {
		t.Errorf("Identifier() = %q, 期望 %q", exec.Identifier(), ProviderKey)
	}
}

func TestGrokExecutorSatisfiesInterfaces(t *testing.T) {
	var x any = NewGrokExecutor(grok.Config{})
	if _, ok := x.(coreauth.ProviderExecutor); !ok {
		t.Error("GrokExecutor 未满足 coreauth.ProviderExecutor")
	}
	exec := NewGrokExecutor(grok.Config{})
	if got := exec.RequestToFormat(clipexec.Request{}, clipexec.Options{}); got.String() != "openai" {
		t.Errorf("RequestToFormat() = %q, 期望 openai", got)
	}
	if exec.Identifier() != GrokProviderKey {
		t.Errorf("Identifier() = %q, 期望 %q", exec.Identifier(), GrokProviderKey)
	}
}
