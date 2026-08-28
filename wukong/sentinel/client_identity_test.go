package sentinel

import (
	"regexp"
	"strings"
	"testing"
)

// majorFrom 从形如 `"Chromium";v="146"` 或 `Chrome/146.0.0.0` 的串里取主版本号。
func majorFrom(t *testing.T, s, pattern string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(s)
	if len(m) < 2 {
		t.Fatalf("从 %q 中按 %q 取不到版本号", s, pattern)
	}
	return m[1]
}

// TestBrowserIdentityConsistent 锁定「浏览器身份自洽」这件事。
// 曾经 UA 自称 Chrome 147 而 sec-ch-ua 写 146，是一眼假的破绽。
func TestBrowserIdentityConsistent(t *testing.T) {
	h := NewClient(Config{BearerToken: "t"}).HTTPClient().Headers

	uaMajor := majorFrom(t, h.Get("User-Agent"), `Chrome/(\d+)\.`)
	edgMajor := majorFrom(t, h.Get("User-Agent"), `Edg/(\d+)\.`)
	hintMajor := majorFrom(t, h.Get("Sec-Ch-Ua"), `"Chromium";v="(\d+)"`)
	listMajor := majorFrom(t, h.Get("Sec-Ch-Ua-Full-Version-List"), `"Chromium";v="(\d+)\.`)
	fullMajor := majorFrom(t, h.Get("Sec-Ch-Ua-Full-Version"), `"(\d+)\.`)

	for name, got := range map[string]string{
		"UA 里的 Edg":                   edgMajor,
		"sec-ch-ua":                   hintMajor,
		"sec-ch-ua-full-version-list": listMajor,
		"sec-ch-ua-full-version":      fullMajor,
	} {
		if got != uaMajor {
			t.Errorf("%s 主版本 = %s，与 UA 的 Chrome/%s 不一致", name, got, uaMajor)
		}
	}
}

// TestImpersonateDoesNotOverrideIdentity 盯住 req 的调用顺序。
// ImpersonateChrome 内部会 SetCommonHeaders，一旦它排在我们后面，
// 整套身份会被悄悄换成它内置的 Chrome 120 / macOS。
func TestImpersonateDoesNotOverrideIdentity(t *testing.T) {
	h := NewClient(Config{BearerToken: "t"}).HTTPClient().Headers

	if ua := h.Get("User-Agent"); ua != defaultUA {
		t.Errorf("User-Agent 被覆盖了：\n 实得 %s\n 期望 %s", ua, defaultUA)
	}
	if got := h.Get("Sec-Ch-Ua-Platform"); got != `"Windows"` {
		t.Errorf("Sec-Ch-Ua-Platform = %s，期望 \"Windows\"（被 req 的 macOS 默认值顶掉了？）", got)
	}
	if strings.Contains(h.Get("User-Agent"), "Macintosh") {
		t.Error("UA 变成 macOS 了，说明 ImpersonateChrome 排在了自定义头之后")
	}
}

// TestCustomUserAgentApplies cfg.UserAgent 必须真的能生效，
// 否则它就是一个骗人的配置项。
func TestCustomUserAgentApplies(t *testing.T) {
	const custom = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/140.0.0.0 Safari/537.36"
	h := NewClient(Config{BearerToken: "t", UserAgent: custom}).HTTPClient().Headers
	if got := h.Get("User-Agent"); got != custom {
		t.Errorf("cfg.UserAgent 没生效：实得 %s", got)
	}
}

// TestXHRFetchMetadata backend-api 都是 fetch 发的 XHR，
// 不该带顶层导航才有的那套 sec-fetch 值。
func TestXHRFetchMetadata(t *testing.T) {
	h := NewClient(Config{BearerToken: "t"}).HTTPClient().Headers

	for k, want := range map[string]string{
		"Sec-Fetch-Dest": "empty",
		"Sec-Fetch-Mode": "cors",
		"Sec-Fetch-Site": "same-origin",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, 期望 %q（导航语义会暴露不是 fetch 发的）", k, got, want)
		}
	}

	for _, k := range navigationOnlyHeaders {
		if got := h.Get(k); got != "" {
			t.Errorf("%s = %q, XHR 不该带这个头", k, got)
		}
	}

	if got := h.Get("Accept"); strings.Contains(got, "text/html") {
		t.Errorf("Accept = %q, 这是导航请求的值，不是 API 调用的", got)
	}
}

// TestPOWFingerprintMatchesSentUA PoW 指纹数组里嵌的 UA 必须和实际发出去的
// User-Agent 一致，否则风控只要比对一下就穿帮了。
func TestPOWFingerprintMatchesSentUA(t *testing.T) {
	c := NewClient(Config{BearerToken: "t"})
	sentUA := c.HTTPClient().Headers.Get("User-Agent")

	if c.userAgent != sentUA {
		t.Errorf("PoW 用的 UA 与实际请求头不一致：\n PoW  %s\n 请求 %s", c.userAgent, sentUA)
	}
}
