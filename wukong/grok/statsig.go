package grok

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

const (
	StatsigModeManual          = "manual"
	StatsigModeURL             = "url"
	StatsigModeLocal           = "local"
	DefaultStatsigSignerURL    = "https://grok.wodf.de/sign"
	statsigMetaBodyLimit       = 4 << 20
	statsigResponseLimit       = 4 << 10
	statsigSignerClientTimeout = 12 * time.Second
)

var errStatsigMetaMissing = fmt.Errorf("Grok index 缺少 grok-site-verification")

type statsigSigner struct {
	mu           sync.Mutex
	materials    *statsigPageMaterials
	source       string
	scripts      int
	indices      statsigByteIndices
	indicesUntil time.Time
}

type StatsigInspect struct {
	PagePath     string
	Source       string
	Scripts      int
	Indices      string
	IndexSource  string
	AnimationKey string
	Hash16       string
	Counter      int64
	Local        bool
	LocalError   string
}

func newStatsigSigner() *statsigSigner { return &statsigSigner{} }

func (s *statsigSigner) invalidate() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.materials = nil
	s.source = ""
	s.scripts = 0
	s.indices = statsigByteIndices{}
	s.indicesUntil = time.Time{}
	s.mu.Unlock()
}

func (s *statsigSigner) noteSource(source string, scripts int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.source = source
	s.scripts = scripts
	s.mu.Unlock()
}

func (c *Client) ProbeStatsig(ctx context.Context) (pagePath string, err error) {
	inspect, err := c.InspectStatsig(ctx)
	return inspect.PagePath, err
}

func (c *Client) InspectStatsig(ctx context.Context) (StatsigInspect, error) {
	inspect := StatsigInspect{}
	_, _, pagePath, _ := c.fetchStatsigPage(ctx)
	inspect.PagePath = pagePath
	target := c.baseURL() + "/rest/app-chat/conversations/new"
	path := statsigRequestPath(target)
	materials, localErr := c.statsigMaterials(ctx)
	if localErr != nil {
		inspect.LocalError = localErr.Error()
	}
	if c.statsig != nil {
		c.statsig.mu.Lock()
		inspect.Source = c.statsig.source
		inspect.Scripts = c.statsig.scripts
		if c.statsig.indices.Source != "" {
			inspect.Indices = c.statsig.indices.String()
			inspect.IndexSource = c.statsig.indices.Source
		}
		c.statsig.mu.Unlock()
	}
	if localErr == nil {
		if materials.indices.Source != "" {
			inspect.Indices = materials.indices.String()
			inspect.IndexSource = materials.indices.Source
		}
		signed, err := signStatsigIDWithTrace(http.MethodPost, path, materials, time.Now().UTC())
		if err == nil && validStatsigID(signed.Value) {
			inspect.Local = true
			inspect.AnimationKey = signed.AnimationKey
			inspect.Hash16 = signed.Hash16
			inspect.Counter = signed.Counter
			if materials.pagePath != "" {
				inspect.PagePath = materials.pagePath
			}
			if inspect.Source == "" {
				inspect.Source = "local"
			}
			if inspect.PagePath == "" {
				inspect.PagePath = "local"
			}
			return inspect, nil
		}
	}
	if !c.statsigAllowsRemote() {
		if localErr != nil {
			return inspect, localErr
		}
		return inspect, fmt.Errorf("本地 Statsig 签名无效")
	}
	signed, err := c.signStatsig(ctx, http.MethodPost, target)
	if err != nil {
		return inspect, err
	}
	if !validStatsigID(signed) {
		return inspect, fmt.Errorf("x-statsig-id 无效")
	}
	inspect.Source = "remote"
	if inspect.PagePath == "" {
		inspect.PagePath = "remote"
	}
	return inspect, nil
}

func (c *Client) applySignedStatsig(ctx context.Context, headers http.Header, method, target string) error {
	if headers == nil {
		return nil
	}
	headers.Del("x-statsig-id")
	if strings.EqualFold(c.cfg.StatsigMode, StatsigModeManual) {
		value := strings.TrimSpace(c.cfg.StatsigManualValue)
		if !validStatsigID(value) {
			return fmt.Errorf("手动 Statsig 配置无效")
		}
		headers.Set("x-statsig-id", value)
		return nil
	}
	signed, err := c.signStatsig(ctx, method, target)
	if err != nil {
		return err
	}
	if signed == "" {
		return fmt.Errorf("Statsig 签名为空")
	}
	headers.Set("x-statsig-id", signed)
	return nil
}

func (c *Client) statsigAllowsRemote() bool {
	return strings.EqualFold(c.cfg.StatsigMode, StatsigModeURL) && strings.TrimSpace(c.cfg.StatsigSignerURL) != ""
}

func (c *Client) signStatsig(ctx context.Context, method, target string) (string, error) {
	if strings.EqualFold(c.cfg.StatsigMode, StatsigModeManual) {
		value := strings.TrimSpace(c.cfg.StatsigManualValue)
		if !validStatsigID(value) {
			return "", fmt.Errorf("手动 Statsig 配置无效")
		}
		return value, nil
	}
	path := statsigRequestPath(target)
	local, localErr := c.signStatsigLocal(ctx, method, path)
	if localErr == nil {
		return local, nil
	}
	if !c.statsigAllowsRemote() {
		return "", localErr
	}
	remote, remoteErr := c.signStatsigRemote(ctx, method, path)
	if remoteErr != nil {
		return "", fmt.Errorf("本地 Statsig 签名失败: %v; 远程回退: %w", localErr, remoteErr)
	}
	return remote, nil
}

func (c *Client) signStatsigLocal(ctx context.Context, method, path string) (string, error) {
	materials, err := c.statsigMaterials(ctx)
	if err != nil {
		return "", err
	}
	return signStatsigID(method, path, materials, time.Now().UTC())
}

func (c *Client) signStatsigRemote(ctx context.Context, method, path string) (string, error) {
	meta, err := c.fetchStatsigMeta(ctx)
	if err != nil {
		return "", err
	}
	signature, err := c.requestStatsigSignature(ctx, method, path, meta)
	if err == nil {
		return signature, nil
	}
	meta, refreshErr := c.fetchStatsigMeta(ctx)
	if refreshErr != nil {
		return "", fmt.Errorf("刷新 Statsig metaContent: %w", refreshErr)
	}
	signature, retryErr := c.requestStatsigSignature(ctx, method, path, meta)
	if retryErr != nil {
		return "", fmt.Errorf("Statsig 签名失败: %w", retryErr)
	}
	return signature, nil
}

func (c *Client) fetchStatsigMeta(ctx context.Context) (string, error) {
	meta, _, _, err := c.fetchStatsigPage(ctx)
	return meta, err
}

func (c *Client) requestStatsigSignature(ctx context.Context, method, path, metaContent string) (string, error) {
	if err := validateStatsigSignerURL(c.cfg.StatsigSignerURL); err != nil {
		return "", err
	}
	payload, err := json.Marshal(map[string]any{
		"method": strings.ToUpper(strings.TrimSpace(method)),
		"path":   path,
		"environment": map[string]string{
			"metaContent": metaContent,
		},
	})
	if err != nil {
		return "", err
	}
	reqCtx, cancel := context.WithTimeout(ctx, statsigSignerClientTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.cfg.StatsigSignerURL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		Timeout: statsigSignerClientTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if proxy := strings.TrimSpace(c.cfg.ProxyURL); proxy != "" {
		if parsed, parseErr := url.Parse(proxy); parseErr == nil {
			client.Transport = &http.Transport{Proxy: http.ProxyURL(parsed)}
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, statsigResponseLimit+1))
	if err != nil {
		return "", err
	}
	if len(body) > statsigResponseLimit {
		return "", fmt.Errorf("签名响应超过安全上限")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("签名服务返回 %d", response.StatusCode)
	}
	var value struct {
		StatsigID string `json:"x-statsig-id"`
	}
	if json.Unmarshal(body, &value) != nil || !validStatsigID(value.StatsigID) {
		return "", fmt.Errorf("签名服务响应无效")
	}
	return value.StatsigID, nil
}

func statsigRequestPath(target string) string {
	path := target
	if parsed, err := url.Parse(target); err == nil && parsed.Path != "" {
		path = parsed.EscapedPath()
		if parsed.RawQuery != "" {
			path += "?" + parsed.RawQuery
		}
	}
	return path
}

func validateStatsigSignerURL(value string) error {
	raw := strings.TrimSpace(value)
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || len(raw) > 2048 {
		return errors.New("签名 URL 必须是无凭据、查询参数和片段的完整地址")
	}
	host := strings.ToLower(parsed.Hostname())
	internal := host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || host == "127.0.0.1" || host == "::1"
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		if internal {
			return nil
		}
	case "https":
		if internal || parsed.Port() == "" || parsed.Port() == "443" {
			return nil
		}
	}
	return errors.New("公网签名 URL 必须使用 HTTPS:443；HTTP 和自定义端口仅允许内网地址")
}

func (c *Client) statsigMaterials(ctx context.Context) (statsigPageMaterials, error) {
	c.statsig.mu.Lock()
	if c.statsig.materials != nil && time.Now().Before(c.statsig.materials.expiresAt) {
		materials := *c.statsig.materials
		c.statsig.mu.Unlock()
		return materials, nil
	}
	c.statsig.mu.Unlock()

	meta, body, pagePath, pageHTML, err := c.loadStatsigPageMaterials(ctx)
	if err != nil && c.clearanceEnabled() && c.ensureClearance(ctx, true) == nil {
		c.statsig.invalidate()
		meta, body, pagePath, pageHTML, err = c.loadStatsigPageMaterials(ctx)
	}
	if err != nil {
		return statsigPageMaterials{}, err
	}
	frames, err := extractStatsigCurves(body)
	if err != nil {
		if file := strings.TrimSpace(c.cfg.StatsigCurvesFile); file != "" {
			frames, err = loadStatsigFramesFile(file)
			if err == nil {
				scripts := 0
				if c.statsig != nil {
					c.statsig.mu.Lock()
					scripts = c.statsig.scripts
					c.statsig.mu.Unlock()
				}
				c.statsig.noteSource("file", scripts)
			}
		}
		if err != nil {
			return statsigPageMaterials{}, err
		}
	}
	indices := c.resolveStatsigIndices(ctx, pageHTML, pagePath)
	materials, err := buildStatsigMaterialsWith(meta, frames, indices, time.Now().UTC())
	if err != nil {
		return statsigPageMaterials{}, err
	}
	materials.pagePath = pagePath
	c.statsig.mu.Lock()
	c.statsig.materials = &materials
	c.statsig.mu.Unlock()
	return materials, nil
}

func (c *Client) loadStatsigPageMaterials(ctx context.Context) (string, []byte, string, []byte, error) {
	meta, body, pagePath, err := c.fetchStatsigPage(ctx)
	if err != nil {
		return "", nil, "", nil, err
	}
	if _, err := extractStatsigCurves(body); err == nil {
		c.statsig.noteSource("html", 0)
		return meta, body, pagePath, body, nil
	}
	extra, source, scripts, collectErr := c.collectStatsigCurveBody(ctx, body, pagePath)
	if collectErr != nil {
		if altMeta, altBody, altPath, altErr := c.fetchStatsigCurvesPage(ctx); altErr == nil {
			c.statsig.noteSource("html", scripts)
			return altMeta, altBody, altPath, body, nil
		}
		c.statsig.noteSource("", scripts)
		if strings.TrimSpace(c.cfg.StatsigCurvesFile) != "" {
			return meta, body, pagePath, body, nil
		}
		return "", nil, pagePath, body, collectErr
	}
	c.statsig.noteSource(source, scripts)
	return meta, extra, pagePath, body, nil
}

func (c *Client) fetchStatsigPage(ctx context.Context) (string, []byte, string, error) {
	var lastErr error
	for _, path := range []string{"/imagine", "/"} {
		reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		headers := http.Header{}
		headers.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		headers.Set("Accept-Encoding", "gzip, deflate, br, zstd")
		headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		headers.Set("Cache-Control", "no-cache")
		headers.Set("Pragma", "no-cache")
		headers.Set("Sec-Fetch-Dest", "document")
		headers.Set("Sec-Fetch-Mode", "navigate")
		headers.Set("Sec-Fetch-Site", "same-origin")
		headers.Set("Upgrade-Insecure-Requests", "1")
		headers.Set("User-Agent", c.userAgent())
		headers.Set("Cookie", c.cookieHeader(""))
		resp, err := c.do(reqCtx, http.MethodGet, c.baseURL()+path, nil, headers)
		cancel()
		if err != nil {
			return "", nil, "", err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, statsigMetaBodyLimit+1))
		_ = resp.Body.Close()
		if readErr != nil {
			return "", nil, "", readErr
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("Grok 页面 %s 返回 %d", path, resp.StatusCode)
			continue
		}
		if int64(len(body)) > statsigMetaBodyLimit {
			lastErr = fmt.Errorf("Grok 页面 %s 超过安全上限", path)
			continue
		}
		meta, extractErr := extractStatsigMetaContent(body)
		if extractErr != nil {
			lastErr = extractErr
			continue
		}
		return meta, body, path, nil
	}
	if lastErr == nil {
		lastErr = errStatsigMetaMissing
	}
	return "", nil, "", lastErr
}

func (c *Client) fetchStatsigCurvesPage(ctx context.Context) (string, []byte, string, error) {
	var lastErr error
	for _, path := range []string{"/imagine", "/"} {
		reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		headers := http.Header{}
		headers.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		headers.Set("Accept-Encoding", "gzip, deflate, br, zstd")
		headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		headers.Set("Cache-Control", "no-cache")
		headers.Set("Pragma", "no-cache")
		headers.Set("Sec-Fetch-Dest", "document")
		headers.Set("Sec-Fetch-Mode", "navigate")
		headers.Set("Sec-Fetch-Site", "same-origin")
		headers.Set("Upgrade-Insecure-Requests", "1")
		headers.Set("User-Agent", c.userAgent())
		headers.Set("Cookie", c.cookieHeader(""))
		resp, err := c.do(reqCtx, http.MethodGet, c.baseURL()+path, nil, headers)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, statsigMetaBodyLimit+1))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("Grok 页面 %s 返回 %d", path, resp.StatusCode)
			continue
		}
		if int64(len(body)) > statsigMetaBodyLimit {
			lastErr = fmt.Errorf("Grok 页面 %s 超过安全上限", path)
			continue
		}
		if _, err := extractStatsigCurves(body); err != nil {
			lastErr = err
			continue
		}
		meta, metaErr := extractStatsigMetaContent(body)
		if metaErr != nil {
			lastErr = metaErr
			continue
		}
		return meta, body, path, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("页面缺少 Botox 曲线")
	}
	return "", nil, "", lastErr
}

func extractStatsigMetaContent(body []byte) (string, error) {
	tokenizer := html.NewTokenizer(bytes.NewReader(body))
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			if tokenizer.Err() == io.EOF {
				return "", errStatsigMetaMissing
			}
			return "", tokenizer.Err()
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttrs := tokenizer.TagName()
			if !strings.EqualFold(string(name), "meta") || !hasAttrs {
				continue
			}
			metaName := ""
			content := ""
			for {
				key, value, more := tokenizer.TagAttr()
				switch strings.ToLower(string(key)) {
				case "name":
					metaName = normalizeStatsigMetaName(string(value))
				case "content":
					content = strings.TrimSpace(string(value))
				}
				if !more {
					break
				}
			}
			if metaName == "grok-site-verification" && content != "" {
				return content, nil
			}
		}
	}
}

func normalizeStatsigMetaName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.NewReplacer("\u2010", "-", "\u2011", "-", "\u2012", "-", "\u2013", "-", "\u2014", "-", "\u2015", "-").Replace(value)
}

func validStatsigID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	decoded, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil {
		decoded, err = base64.StdEncoding.DecodeString(value)
	}
	return err == nil && len(decoded) == 70
}
