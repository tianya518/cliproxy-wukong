package grok

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// 曲线扫描保持小预算：RSC 命中时根本不会走到脚本爬取。
	statsigMaxScriptFetches = 32
	// 下标在懒加载签名器里。2026-08-29 的 /imagine 页面有 200+ 个 chunk 引用，
	// 含 botoxSign 的入口排在第 60 名之后，模块映射表更靠后；32 条上限会让发现
	// 永远停在默认值上。收集与抓取分开限额，避免把「页面里有多少 URL」和
	// 「最多 GET 多少次」绑死。
	statsigMaxScriptDiscover     = 512
	statsigMaxIndexScriptFetches = 192
	statsigScriptBodyLimit       = 2 << 20
)

var (
	statsigAbsoluteScriptPattern = regexp.MustCompile(`(?i)https://(?:cdn\.)?grok\.com/_next/static/[^"'\\s>]+`)
	statsigRelativeScriptPattern = regexp.MustCompile(`(?i)(?:src|href)=["']([^"']+_next/static/[^"']+)["']`)
	statsigQuotedScriptPattern   = regexp.MustCompile(`(?i)["'](/_next/static/[^"']+\.(?:js|txt))["']`)
)

type StatsigCurveScan struct {
	Scripts        int
	Curves         int
	SVG            int
	Anim           int
	Hits           []string
	HTMLBytes      int
	FlightBytes    int
	HasNextFlight  bool
	HasBotoxFooter bool
	HasEscapedJSON bool
	Title          string
}

func (c *Client) ScanStatsigCurveHints(ctx context.Context) (StatsigCurveScan, error) {
	_, html, pagePath, err := c.fetchStatsigPage(ctx)
	if err != nil {
		return StatsigCurveScan{}, err
	}
	pageURL := strings.TrimRight(c.baseURL(), "/") + pagePath
	queue := extractStatsigScriptURLs(html, pageURL)
	scan := StatsigCurveScan{}
	countHints := func(label string, body []byte) {
		text := string(body)
		hasCurves := strings.Contains(text, `"curves":`)
		hasSVG := strings.Contains(text, statsigSVGPathPrefix)
		hasAnim := strings.Contains(text, "loading-x-anim")
		if hasCurves {
			scan.Curves++
		}
		if hasSVG {
			scan.SVG++
		}
		if hasAnim {
			scan.Anim++
		}
		if hasCurves || hasSVG || hasAnim {
			scan.Hits = append(scan.Hits, label)
		}
	}
	countHints("html:"+pagePath, html)
	scan.HTMLBytes = len(html)
	rawHTML := string(html)
	scan.HasNextFlight = strings.Contains(rawHTML, nextFlightPushPrefix)
	scan.HasBotoxFooter = strings.Contains(rawHTML, "BotoxFooter")
	scan.HasEscapedJSON = strings.Contains(rawHTML, `\"curves\":`) || strings.Contains(rawHTML, `"curves":`)
	if start := strings.Index(strings.ToLower(rawHTML), "<title>"); start >= 0 {
		rest := rawHTML[start+7:]
		if end := strings.Index(strings.ToLower(rest), "</title>"); end >= 0 && end <= 80 {
			scan.Title = strings.TrimSpace(rest[:end])
		}
	}
	if flight := extractNextFlightText(rawHTML); flight != "" {
		scan.FlightBytes = len(flight)
		countHints("flight:"+pagePath, []byte(flight))
	}
	seen := make(map[string]struct{}, len(queue))
	for _, raw := range queue {
		seen[raw] = struct{}{}
	}
	limit := statsigMaxScriptFetches * 2
	for len(queue) > 0 && scan.Scripts < limit {
		raw := queue[0]
		queue = queue[1:]
		body, fetchErr := c.fetchStatsigScript(ctx, raw)
		scan.Scripts++
		if fetchErr != nil {
			continue
		}
		countHints(raw, body)
		for _, next := range extractStatsigScriptURLs(body, pageURL) {
			if _, exists := seen[next]; exists || len(seen) >= limit {
				continue
			}
			seen[next] = struct{}{}
			queue = append(queue, next)
		}
	}
	return scan, nil
}

func (c *Client) collectStatsigCurveBody(ctx context.Context, html []byte, pagePath string) ([]byte, string, int, error) {
	if rsc, err := c.fetchStatsigRSC(ctx); err == nil {
		if _, extractErr := extractStatsigCurves(rsc); extractErr == nil {
			return rsc, "rsc", 0, nil
		}
	}
	pageURL := strings.TrimRight(c.baseURL(), "/") + pagePath
	queue := extractStatsigScriptURLs(html, pageURL)
	seen := make(map[string]struct{}, len(queue))
	for _, raw := range queue {
		seen[raw] = struct{}{}
	}
	scanned := 0
	for len(queue) > 0 && scanned < statsigMaxScriptFetches {
		raw := queue[0]
		queue = queue[1:]
		body, err := c.fetchStatsigScript(ctx, raw)
		scanned++
		if err != nil {
			continue
		}
		if _, extractErr := extractStatsigCurves(body); extractErr == nil {
			return body, "chunk", scanned, nil
		}
		for _, next := range extractStatsigScriptURLs(body, pageURL) {
			if _, exists := seen[next]; exists || len(seen) >= statsigMaxScriptFetches {
				continue
			}
			seen[next] = struct{}{}
			queue = append(queue, next)
		}
	}
	return nil, "", scanned, fmt.Errorf("页面缺少 Botox 曲线")
}

func (c *Client) fetchStatsigRSC(ctx context.Context) ([]byte, error) {
	var lastErr error
	for _, path := range []string{"/imagine", "/"} {
		reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		headers := http.Header{}
		headers.Set("Accept", "text/x-component,*/*;q=0.8")
		headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		headers.Set("Cache-Control", "no-cache")
		headers.Set("Pragma", "no-cache")
		headers.Set("RSC", "1")
		headers.Set("Next-Url", path)
		headers.Set("Next-Router-Prefetch", "1")
		headers.Set("Sec-Fetch-Dest", "empty")
		headers.Set("Sec-Fetch-Mode", "cors")
		headers.Set("Sec-Fetch-Site", "same-origin")
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
			lastErr = fmt.Errorf("Grok RSC %s 返回 %d", path, resp.StatusCode)
			continue
		}
		if int64(len(body)) > statsigMetaBodyLimit {
			lastErr = fmt.Errorf("Grok RSC %s 超过安全上限", path)
			continue
		}
		if _, err := extractStatsigCurves(body); err != nil {
			lastErr = err
			continue
		}
		return body, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("页面缺少 Botox 曲线")
	}
	return nil, lastErr
}

func (c *Client) fetchStatsigScript(ctx context.Context, rawURL string) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	headers := http.Header{}
	headers.Set("Accept", "*/*")
	headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Pragma", "no-cache")
	headers.Set("Referer", strings.TrimRight(c.baseURL(), "/")+"/imagine")
	headers.Set("Sec-Fetch-Dest", "script")
	headers.Set("Sec-Fetch-Mode", "no-cors")
	headers.Set("Sec-Fetch-Site", "same-site")
	headers.Set("User-Agent", c.userAgent())
	if scriptHostBelongsTo(rawURL, c.baseURL()) {
		headers.Set("Cookie", c.cookieHeader(""))
	}
	resp, err := c.do(reqCtx, http.MethodGet, rawURL, nil, headers)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, statsigScriptBodyLimit+1))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Statsig 脚本返回 %d", resp.StatusCode)
	}
	if int64(len(body)) > statsigScriptBodyLimit {
		return nil, fmt.Errorf("Statsig 脚本超过安全上限")
	}
	return body, nil
}

func extractStatsigScriptURLs(body []byte, pageURL string) []string {
	return extractStatsigScriptURLsN(body, pageURL, statsigMaxScriptFetches)
}

func extractStatsigScriptURLsN(body []byte, pageURL string, limit int) []string {
	if limit <= 0 {
		limit = statsigMaxScriptDiscover
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, 16)
	add := func(raw string) {
		if len(out) >= limit {
			return
		}
		resolved, ok := trustedStatsigScriptURL(raw, pageURL)
		if !ok {
			return
		}
		if _, exists := seen[resolved]; exists {
			return
		}
		seen[resolved] = struct{}{}
		out = append(out, resolved)
	}
	for _, match := range statsigAbsoluteScriptPattern.FindAllString(string(body), -1) {
		add(match)
		if len(out) >= limit {
			return out
		}
	}
	for _, match := range statsigRelativeScriptPattern.FindAllStringSubmatch(string(body), -1) {
		if len(match) > 1 {
			add(match[1])
		}
		if len(out) >= limit {
			return out
		}
	}
	for _, match := range statsigQuotedScriptPattern.FindAllStringSubmatch(string(body), -1) {
		if len(match) > 1 {
			add(match[1])
		}
		if len(out) >= limit {
			return out
		}
	}
	return out
}

func trustedStatsigScriptURL(raw, pageURL string) (string, bool) {
	raw = strings.TrimSpace(strings.Trim(raw, `"'`))
	if raw == "" || strings.HasPrefix(raw, "data:") || strings.HasPrefix(raw, "javascript:") {
		return "", false
	}
	base, err := url.Parse(pageURL)
	if err != nil || base.Host == "" {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	resolved := base.ResolveReference(parsed)
	if resolved.User != nil || resolved.Fragment != "" {
		return "", false
	}
	scheme := strings.ToLower(resolved.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	host := strings.ToLower(resolved.Hostname())
	path := resolved.EscapedPath()
	if !strings.Contains(path, "/_next/static/") || !(strings.HasSuffix(path, ".js") || strings.HasSuffix(path, ".txt")) {
		return "", false
	}
	if host == "cdn.grok.com" || host == "grok.com" || host == "www.grok.com" {
		return resolved.String(), true
	}
	if strings.EqualFold(host, base.Hostname()) {
		return resolved.String(), true
	}
	return "", false
}

func scriptHostBelongsTo(rawURL, pageBase string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	base, err := url.Parse(pageBase)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Hostname(), base.Hostname())
}
