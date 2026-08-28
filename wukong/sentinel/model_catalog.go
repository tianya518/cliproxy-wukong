package sentinel

// model_catalog.go —— 运行时从官网 /backend-api/models 拉取权威模型目录。
//
// 官网模型阵容变动频繁（2026 年内已改版两次：5.3/5.4/5.5 三族 → 5.5/5.6 两族），
// 硬编码映射注定滞后。这里在运行时拉一次官方列表，由它驱动 model 解析；
// 拉取失败时退回 model_resolve.go 的静态表，保证无凭证/离线场景仍可用。
//
// 目录里真正有用的是三个字段：
//   - reasoning_type               none（极速）| auto（自动）| reasoning（思考）
//   - configurable_thinking_effort 该 slug 是否接受 thinking_effort
//   - thinking_efforts             接受哪些取值（当前为 standard、extended）
//
// 官网 UI 上的「推理强度 3 档」并不是三个 thinking_effort 值，而是
// 「换 slug + 换 effort」的组合：极速走 -instant slug，中/高走 -thinking slug
// 配 standard/extended。

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// CatalogModel 官网模型目录中的一项。
type CatalogModel struct {
	Slug  string
	Title string
	// ReasoningType 为 none | auto | reasoning。
	ReasoningType string
	// ConfigurableThinkingEffort 为 true 时该 slug 接受 thinking_effort 字段。
	ConfigurableThinkingEffort bool
	// ThinkingEfforts 支持的强度取值，保持官网顺序（第一个即默认档）。
	ThinkingEfforts []string
	// IsWorkModeModel 为 true 时这是官网「工作」标签页的模型（slug 带 -wm），
	// 不是「聊天」标签页用的。同一套 /f/conversation 协议也能打通，但额度、
	// 产品面、默认工具编排都跟聊天分开，不能当普通 chat 模型对外暴露。
	IsWorkModeModel bool
}

// DefaultEffort 返回该模型应写入请求体的 thinking_effort。
// 不可配置强度的模型返回空串，表示请求体不携带该字段。
func (m CatalogModel) DefaultEffort() string {
	if !m.ConfigurableThinkingEffort || len(m.ThinkingEfforts) == 0 {
		return ""
	}
	return m.ThinkingEfforts[0]
}

// HideFromChatCatalog 为 true 时不要出现在 /v1/models 和网关模型表里。
// 调用方仍可按 slug 显式指定这些模型；这里只控制「默认暴露」。
func (m CatalogModel) HideFromChatCatalog() bool {
	if m.IsWorkModeModel {
		return true
	}
	return strings.EqualFold(m.Slug, "research")
}

// SupportsEffort 判断该模型是否接受指定强度。
func (m CatalogModel) SupportsEffort(effort string) bool {
	for _, e := range m.ThinkingEfforts {
		if strings.EqualFold(e, effort) {
			return true
		}
	}
	return false
}

// ModelCatalog 一次拉取得到的完整模型目录。
type ModelCatalog struct {
	Models    []CatalogModel
	FetchedAt time.Time

	bySlug map[string]CatalogModel
}

// Lookup 按 slug 查模型。除精确匹配外，还会尝试把点号换成连字符
// （官网既有 gpt-5-6 这种连字符 slug，也有 gpt-5.6-sol-wm 这种带点的）。
func (c *ModelCatalog) Lookup(slug string) (CatalogModel, bool) {
	if c == nil || len(c.bySlug) == 0 {
		return CatalogModel{}, false
	}
	key := strings.ToLower(strings.TrimSpace(slug))
	if m, ok := c.bySlug[key]; ok {
		return m, true
	}
	if m, ok := c.bySlug[strings.ReplaceAll(key, ".", "-")]; ok {
		return m, true
	}
	return CatalogModel{}, false
}

// Slugs 返回目录中全部 slug，保持官网顺序。
func (c *ModelCatalog) Slugs() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.Models))
	for _, m := range c.Models {
		out = append(out, m.Slug)
	}
	return out
}

func (c *ModelCatalog) index() {
	c.bySlug = make(map[string]CatalogModel, len(c.Models))
	for _, m := range c.Models {
		c.bySlug[strings.ToLower(m.Slug)] = m
	}
}

// ─── 全局缓存 ────────────────────────────────────────────────────────────────
//
// 目录与账号无关，全进程共用一份即可；解析路径只读，拉取协程写。

var (
	catalogMu      sync.RWMutex
	currentCatalog *ModelCatalog
)

// SetModelCatalog 安装（或替换）全局模型目录。传 nil 表示回退到静态表。
func SetModelCatalog(c *ModelCatalog) {
	catalogMu.Lock()
	currentCatalog = c
	catalogMu.Unlock()
}

// CurrentModelCatalog 返回当前全局模型目录，未拉取时为 nil。
func CurrentModelCatalog() *ModelCatalog {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	return currentCatalog
}

// ─── 拉取 ────────────────────────────────────────────────────────────────────

// modelsResponse 只声明我们关心的字段，其余（attachments、enabled_tools 等）忽略。
type modelsResponse struct {
	Models []struct {
		Slug                       string `json:"slug"`
		Title                      string `json:"title"`
		ReasoningType              string `json:"reasoning_type"`
		ConfigurableThinkingEffort bool   `json:"configurable_thinking_effort"`
		IsWorkModeModel            bool   `json:"is_work_mode_model"`
		ThinkingEfforts            []struct {
			ThinkingEffort string `json:"thinking_effort"`
		} `json:"thinking_efforts"`
	} `json:"models"`
}

// FetchModelCatalog 拉取官网模型目录。只需 Bearer token，不涉及 Sentinel/PoW。
func (c *Client) FetchModelCatalog() (*ModelCatalog, error) {
	resp, err := c.httpClient.R().Get("/backend-api/models?history_and_training_disabled=false")
	if err != nil {
		return nil, fmt.Errorf("fetch models: %w", err)
	}
	if resp.StatusCode != 200 {
		body := resp.String()
		if len(body) > 200 {
			body = body[:200]
		}
		return nil, fmt.Errorf("fetch models: http %d: %s", resp.StatusCode, body)
	}

	var raw modelsResponse
	if err = json.Unmarshal(resp.Bytes(), &raw); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}
	if len(raw.Models) == 0 {
		return nil, fmt.Errorf("parse models: empty list")
	}

	cat := &ModelCatalog{
		Models:    make([]CatalogModel, 0, len(raw.Models)),
		FetchedAt: time.Now(),
	}
	for _, m := range raw.Models {
		if strings.TrimSpace(m.Slug) == "" {
			continue
		}
		efforts := make([]string, 0, len(m.ThinkingEfforts))
		for _, e := range m.ThinkingEfforts {
			if e.ThinkingEffort != "" {
				efforts = append(efforts, e.ThinkingEffort)
			}
		}
		cat.Models = append(cat.Models, CatalogModel{
			Slug:                       m.Slug,
			Title:                      m.Title,
			ReasoningType:              m.ReasoningType,
			ConfigurableThinkingEffort: m.ConfigurableThinkingEffort,
			ThinkingEfforts:            efforts,
			IsWorkModeModel:            m.IsWorkModeModel,
		})
	}
	if len(cat.Models) == 0 {
		return nil, fmt.Errorf("parse models: no usable slug")
	}
	cat.index()
	return cat, nil
}
