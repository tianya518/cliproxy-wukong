package grok

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// grok.com botoxSign 里写死的 meta 下标（不是从 48 字节 meta 算出来的）。
// 前端发版会改这些字面量；运行时从懒加载签名器 JS 抽，抽不到才用这里最近一次对照成功的值。
// 当前值于 2026-08-29 对照 grok.com/imagine 真实签名（详见 statsig_compare_test.go 的 live 向量）。
// 签名器本体重度混淆，但下标仍是明文；discoverStatsigIndices 会跟完
// botoxSign → 模块 ID → chunk 映射 → 懒加载签名器。上一版 5/12/18,11,46
// 以及中间的 5/22/9,11,20 已失效，表现为 conversations/new 返回 403 code 7。
const (
	statsigSVGIndexByte = 5
	statsigRowIndexByte = 39
	statsigTimeIndexA   = 28
	statsigTimeIndexB   = 36
	statsigTimeIndexC   = 41
)

type statsigByteIndices struct {
	SVG    int
	Row    int
	TimeA  int
	TimeB  int
	TimeC  int
	Source string
}

type statsigIndexHit struct {
	n, start, end int
}

var (
	statsigIndexMod4Pattern  = regexp.MustCompile(`\[\s*(\d{1,2})\s*\]\s*%\s*4\b`)
	statsigIndexMod16Pattern = regexp.MustCompile(`\[\s*(\d{1,2})\s*\]\s*(?:%\s*16\b|\s*,\s*16\s*\))`)
	statsigModuleLoadPattern = regexp.MustCompile(`\.A\((\d{3,8})\)`)
	statsigChunkPathPattern  = regexp.MustCompile(`["']((?:/_next/)?static/chunks/[^"']+\.js)["']`)
)

func defaultStatsigByteIndices() statsigByteIndices {
	return statsigByteIndices{
		SVG:    statsigSVGIndexByte,
		Row:    statsigRowIndexByte,
		TimeA:  statsigTimeIndexA,
		TimeB:  statsigTimeIndexB,
		TimeC:  statsigTimeIndexC,
		Source: "default",
	}
}

func (i statsigByteIndices) String() string {
	return fmt.Sprintf("%d/%d/%d,%d,%d", i.SVG, i.Row, i.TimeA, i.TimeB, i.TimeC)
}

func extractStatsigByteIndices(src []byte) (statsigByteIndices, bool) {
	text := string(src)
	hits := extractStatsigMod16Hits(text)
	for i := 0; i+2 < len(hits); i++ {
		if !statsigHitsAreProduct(text, hits[i], hits[i+1], hits[i+2]) {
			continue
		}
		row := pickStatsigRowHit(hits, i)
		if row < 0 {
			continue
		}
		svg := pickStatsigSVGIndex(text, hits[i].start)
		if svg < 0 {
			continue
		}
		return statsigByteIndices{
			SVG:   svg,
			Row:   row,
			TimeA: hits[i].n,
			TimeB: hits[i+1].n,
			TimeC: hits[i+2].n,
		}, true
	}
	for i := 0; i+3 < len(hits); i++ {
		if hits[i+3].start-hits[i].start > 400 {
			continue
		}
		svg := pickStatsigSVGIndex(text, hits[i].start)
		if svg < 0 {
			continue
		}
		return statsigByteIndices{
			SVG:   svg,
			Row:   hits[i].n,
			TimeA: hits[i+1].n,
			TimeB: hits[i+2].n,
			TimeC: hits[i+3].n,
		}, true
	}
	return statsigByteIndices{}, false
}

func extractStatsigMod16Hits(text string) []statsigIndexHit {
	matches := statsigIndexMod16Pattern.FindAllStringSubmatchIndex(text, -1)
	hits := make([]statsigIndexHit, 0, len(matches))
	for _, match := range matches {
		n, err := strconv.Atoi(text[match[2]:match[3]])
		if err != nil || n < 0 || n >= statsigKeyByteCount {
			continue
		}
		hits = append(hits, statsigIndexHit{n: n, start: match[0], end: match[1]})
	}
	return hits
}

func statsigHitsAreProduct(text string, a, b, c statsigIndexHit) bool {
	return statsigSpanLooksLikeMultiply(text, a.end, b.start) && statsigSpanLooksLikeMultiply(text, b.end, c.start)
}

func statsigSpanLooksLikeMultiply(text string, from, to int) bool {
	if from < 0 || to > len(text) || from > to || to-from > 48 {
		return false
	}
	return strings.Contains(text[from:to], "*")
}

func pickStatsigRowHit(hits []statsigIndexHit, product int) int {
	if product > 0 && hits[product].start-hits[product-1].start <= 600 {
		return hits[product-1].n
	}
	after := product + 3
	if after < len(hits) && hits[after].start-hits[product+2].end <= 600 {
		return hits[after].n
	}
	return -1
}

func pickStatsigSVGIndex(text string, near int) int {
	matches := statsigIndexMod4Pattern.FindAllStringSubmatchIndex(text, -1)
	best, bestDist := -1, -1
	for _, match := range matches {
		n, err := strconv.Atoi(text[match[2]:match[3]])
		if err != nil || n < 0 || n >= statsigKeyByteCount {
			continue
		}
		dist := match[0] - near
		if dist < 0 {
			dist = -dist
		}
		if best < 0 || dist < bestDist {
			best, bestDist = n, dist
		}
	}
	if best >= 0 && bestDist <= 4000 {
		return best
	}
	if len(matches) == 0 {
		return -1
	}
	n, err := strconv.Atoi(text[matches[0][2]:matches[0][3]])
	if err != nil || n < 0 || n >= statsigKeyByteCount {
		return -1
	}
	return n
}

func extractBotoxModuleID(src string) string {
	const needle = "botoxSign"
	start := 0
	for {
		index := strings.Index(src[start:], needle)
		if index < 0 {
			return ""
		}
		index += start
		from := index - 400
		if from < 0 {
			from = 0
		}
		to := index + 80
		if to > len(src) {
			to = len(src)
		}
		if match := statsigModuleLoadPattern.FindStringSubmatch(src[from:to]); len(match) > 1 {
			return match[1]
		}
		start = index + len(needle)
	}
}

func extractModuleChunkPath(src, id string) string {
	if id == "" {
		return ""
	}
	from := 0
	for {
		index := indexBoundedNumber(src, id, from)
		if index < 0 {
			return ""
		}
		windowEnd := index + 500
		if windowEnd > len(src) {
			windowEnd = len(src)
		}
		if match := statsigChunkPathPattern.FindStringSubmatch(src[index:windowEnd]); len(match) > 1 {
			return match[1]
		}
		from = index + len(id)
	}
}

func indexBoundedNumber(src, id string, from int) int {
	for {
		index := strings.Index(src[from:], id)
		if index < 0 {
			return -1
		}
		index += from
		leftOK := index == 0 || src[index-1] < '0' || src[index-1] > '9'
		right := index + len(id)
		rightOK := right >= len(src) || src[right] < '0' || src[right] > '9'
		if leftOK && rightOK {
			return index
		}
		from = index + len(id)
	}
}

func resolveStatsigChunkRef(raw, pageURL string) (string, bool) {
	raw = strings.TrimSpace(strings.Trim(raw, `"'`))
	switch {
	case strings.HasPrefix(raw, "static/chunks/"):
		raw = "/_next/" + raw
	case strings.HasPrefix(raw, "/static/chunks/"):
		raw = "/_next" + raw
	}
	return trustedStatsigScriptURL(raw, pageURL)
}

func (c *Client) resolveStatsigIndices(ctx context.Context, pageHTML []byte, pagePath string) statsigByteIndices {
	if c.statsig != nil {
		c.statsig.mu.Lock()
		cached := !c.statsig.indicesUntil.IsZero() && time.Now().Before(c.statsig.indicesUntil) && c.statsig.indices.Source != ""
		if cached {
			idx := c.statsig.indices
			c.statsig.mu.Unlock()
			return idx
		}
		c.statsig.mu.Unlock()
	}
	idx := defaultStatsigByteIndices()
	if discovered, ok := c.discoverStatsigIndices(ctx, pageHTML, pagePath); ok {
		idx = discovered
	}
	if c.statsig != nil {
		c.statsig.mu.Lock()
		c.statsig.indices = idx
		c.statsig.indicesUntil = time.Now().Add(statsigMaterialsTTL)
		c.statsig.mu.Unlock()
	}
	return idx
}

func (c *Client) discoverStatsigIndices(ctx context.Context, pageHTML []byte, pagePath string) (statsigByteIndices, bool) {
	idx, _, ok := c.discoverStatsigIndicesCounted(ctx, pageHTML, pagePath)
	return idx, ok
}

func (c *Client) discoverStatsigIndicesCounted(ctx context.Context, pageHTML []byte, pagePath string) (statsigByteIndices, int, bool) {
	if c == nil {
		idx, fetches, ok := discoverStatsigIndicesWithFetch(ctx, pageHTML, statsigPageURL("", pagePath), nil)
		return idx, fetches, ok
	}
	pageURL := statsigPageURL(c.baseURL(), pagePath)
	return discoverStatsigIndicesWithFetch(ctx, pageHTML, pageURL, c.fetchStatsigScript)
}

func statsigPageURL(baseURL, pagePath string) string {
	base := strings.TrimRight(baseURL, "/")
	if pagePath == "" {
		return base + "/imagine"
	}
	return base + pagePath
}

func discoverStatsigIndicesWithFetch(ctx context.Context, pageHTML []byte, pageURL string, fetch func(context.Context, string) ([]byte, error)) (statsigByteIndices, int, bool) {
	if idx, ok := extractStatsigByteIndices(pageHTML); ok {
		idx.Source = "html"
		return idx, 0, true
	}
	if fetch == nil {
		return statsigByteIndices{}, 0, false
	}
	if pageURL == "" {
		pageURL = "https://grok.com/imagine"
	}
	queue := extractStatsigScriptURLsN(pageHTML, pageURL, statsigMaxScriptDiscover)
	seen := make(map[string]struct{}, len(queue)+8)
	for _, raw := range queue {
		seen[raw] = struct{}{}
	}
	fetched := make(map[string][]byte, 16)
	moduleIDs := map[string]struct{}{}
	enqueue := func(raw string, front bool) {
		if raw == "" {
			return
		}
		if _, exists := seen[raw]; exists {
			return
		}
		if !front && len(seen) >= statsigMaxScriptDiscover {
			return
		}
		seen[raw] = struct{}{}
		if front {
			queue = append([]string{raw}, queue...)
			return
		}
		queue = append(queue, raw)
	}
	consider := func(body []byte) (statsigByteIndices, bool) {
		if id := extractBotoxModuleID(string(body)); id != "" {
			moduleIDs[id] = struct{}{}
		}
		if idx, ok := extractStatsigByteIndices(body); ok {
			idx.Source = "js"
			return idx, true
		}
		return statsigByteIndices{}, false
	}
	mapChunks := func() {
		scan := func(src string) {
			for id := range moduleIDs {
				path := extractModuleChunkPath(src, id)
				if resolved, ok := resolveStatsigChunkRef(path, pageURL); ok {
					enqueue(resolved, true)
				}
			}
		}
		scan(string(pageHTML))
		for _, body := range fetched {
			scan(string(body))
		}
	}

	if id := extractBotoxModuleID(string(pageHTML)); id != "" {
		moduleIDs[id] = struct{}{}
		mapChunks()
	}

	attempts := 0
	for len(queue) > 0 && attempts < statsigMaxIndexScriptFetches {
		raw := queue[0]
		queue = queue[1:]
		body, err := fetch(ctx, raw)
		attempts++
		if err != nil {
			continue
		}
		fetched[raw] = body
		if idx, ok := consider(body); ok {
			return idx, attempts, true
		}
		for _, next := range extractStatsigScriptURLsN(body, pageURL, statsigMaxScriptDiscover) {
			enqueue(next, false)
		}
		mapChunks()
	}
	return statsigByteIndices{}, attempts, false
}
