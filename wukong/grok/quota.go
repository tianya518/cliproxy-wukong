package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type QuotaWindow struct {
	Mode          string     `json:"mode"`
	Remaining     int        `json:"remaining"`
	Total         int        `json:"total"`
	WindowSeconds int        `json:"window_seconds"`
	ResetAt       *time.Time `json:"reset_at,omitempty"`
	Available     *bool      `json:"available,omitempty"`
}

type QuotaSnapshot struct {
	Tier     Tier          `json:"tier"`
	Windows  []QuotaWindow `json:"windows"`
	SyncedAt time.Time     `json:"synced_at"`
}

// AccountQuotaResult 是单个账号的额度快照。批量查询时单个账号失败只记 Error，不中断整批。
// Windows 是 /rest/rate-limits 的滚动窗口；Billing 是订阅层面的周/月额度，拿不到只记 BillingError。
type AccountQuotaResult struct {
	ID           string           `json:"id"`
	Name         string           `json:"name,omitempty"`
	Tier         Tier             `json:"tier,omitempty"`
	Windows      []QuotaWindow    `json:"windows,omitempty"`
	Billing      *BillingSnapshot `json:"billing,omitempty"`
	BillingError string           `json:"billing_error,omitempty"`
	SyncedAt     *time.Time       `json:"synced_at,omitempty"`
	Error        string           `json:"error,omitempty"`
}

// QuotaFor 取单个账号的额度快照。
func QuotaFor(ctx context.Context, cfg Config, cred Credential) AccountQuotaResult {
	result := AccountQuotaResult{ID: cred.ID(), Name: cred.Name, Tier: cred.WebTier()}
	client := NewClient(cfg, cred)
	snapshot, err := client.SyncQuota(ctx)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	synced := snapshot.SyncedAt
	result.Tier = snapshot.Tier
	result.Windows = snapshot.Windows
	result.SyncedAt = &synced
	if billing, billingErr := client.SyncBilling(ctx); billingErr != nil {
		result.BillingError = billingErr.Error()
	} else {
		result.Billing = &billing
	}
	return result
}

func (c *Client) SyncQuota(ctx context.Context) (QuotaSnapshot, error) {
	// 冷启动的 Statsig 材料抓取（实测 30s+）单独预热，不占每次额度 POST 的 25s 预算。
	if err := c.warmStatsig(ctx); err != nil {
		return QuotaSnapshot{}, err
	}
	windows := make([]QuotaWindow, 0, 6)
	for _, mode := range []string{"auto", "fast"} {
		window, err := c.SyncQuotaMode(ctx, mode)
		if err != nil {
			return QuotaSnapshot{}, err
		}
		windows = append(windows, window)
	}
	imagine, err := c.SyncImagineQuota(ctx)
	if err != nil {
		return QuotaSnapshot{}, err
	}
	windows = append(windows, imagine...)
	return QuotaSnapshot{Tier: c.cred.WebTier(), Windows: windows, SyncedAt: time.Now().UTC()}, nil
}

func (c *Client) SyncQuotaMode(ctx context.Context, mode string) (QuotaWindow, error) {
	payload, _ := json.Marshal(map[string]string{"modelName": mode})
	resp, err := c.doJSON(ctx, c.baseURL()+"/rest/rate-limits", payload, c.baseURL()+"/", c.cfg.QuotaTimeout, true)
	if err != nil {
		return QuotaWindow{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return QuotaWindow{}, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return QuotaWindow{}, ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return QuotaWindow{}, fmt.Errorf("Grok Web 额度接口返回 %d", resp.StatusCode)
	}
	var value struct {
		WindowSizeSeconds int  `json:"windowSizeSeconds"`
		RemainingQueries  *int `json:"remainingQueries"`
		RemainingTokens   *int `json:"remainingTokens"`
		TotalQueries      *int `json:"totalQueries"`
		TotalTokens       *int `json:"totalTokens"`
		WaitTimeSeconds   *int `json:"waitTimeSeconds"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return QuotaWindow{}, err
	}
	// 官网改版后 remainingQueries/totalQueries 有可能换成 *Tokens 命名，两套都认。
	remaining := firstNonNilInt(value.RemainingQueries, value.RemainingTokens)
	total := firstNonNilInt(value.TotalQueries, value.TotalTokens)
	if remaining == nil && total == nil {
		return QuotaWindow{}, fmt.Errorf("Grok Web 额度响应缺少 remainingQueries/totalQueries")
	}
	if value.WindowSizeSeconds <= 0 {
		value.WindowSizeSeconds = 7200
	}
	window := QuotaWindow{Mode: mode, WindowSeconds: value.WindowSizeSeconds}
	if remaining != nil {
		window.Remaining = max(0, *remaining)
	}
	if total != nil {
		window.Total = max(0, *total)
	}
	// 上游平时不给重置时间（滚动窗口也算不出来），只有被限流时才带 waitTimeSeconds。
	// 之前用 now+windowSize 硬造一个，满额度也显示"2 小时后重置"，是误导。
	if value.WaitTimeSeconds != nil && *value.WaitTimeSeconds > 0 {
		resetAt := time.Now().UTC().Add(time.Duration(*value.WaitTimeSeconds) * time.Second)
		window.ResetAt = &resetAt
	}
	return window, nil
}

func firstNonNilInt(values ...*int) *int {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

type imagineQuotaProduct struct {
	Available         *bool      `json:"available"`
	RemainingQueries  *int       `json:"remainingQueries"`
	WindowSizeSeconds *int       `json:"windowSizeSeconds"`
	NextAvailableAt   *time.Time `json:"nextAvailableAt"`
}

func (c *Client) SyncImagineQuota(ctx context.Context) ([]QuotaWindow, error) {
	resp, err := c.doJSON(ctx, c.baseURL()+"/rest/media/imagine/quota_info", []byte("{}"), c.baseURL()+"/imagine", c.cfg.QuotaTimeout, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Grok Web Imagine 配额接口返回 %d", resp.StatusCode)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("解析 Grok Web Imagine 配额: %w", err)
	}
	products := []struct {
		field string
		mode  string
	}{
		{"image", "image"},
		{"imagePro", "image_pro"},
		{"imageEdit", "image_edit"},
		{"video", "video"},
		{"video720p", "video_720p"},
	}
	windows := make([]QuotaWindow, 0, len(products))
	for _, item := range products {
		raw, ok := fields[item.field]
		if !ok {
			continue
		}
		var product imagineQuotaProduct
		if json.Unmarshal(raw, &product) != nil || product.Available == nil {
			continue
		}
		window := QuotaWindow{Mode: item.mode, Available: product.Available, ResetAt: product.NextAvailableAt}
		if product.RemainingQueries != nil {
			window.Remaining = *product.RemainingQueries
		}
		if product.WindowSizeSeconds != nil {
			window.WindowSeconds = *product.WindowSizeSeconds
		}
		windows = append(windows, window)
	}
	return windows, nil
}
