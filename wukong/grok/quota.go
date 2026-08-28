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

func (c *Client) SyncQuota(ctx context.Context) (QuotaSnapshot, error) {
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
		WindowSizeSeconds int `json:"windowSizeSeconds"`
		RemainingQueries  int `json:"remainingQueries"`
		TotalQueries      int `json:"totalQueries"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return QuotaWindow{}, err
	}
	if value.TotalQueries <= 0 {
		return QuotaWindow{}, fmt.Errorf("Grok Web 额度响应缺少 totalQueries")
	}
	if value.WindowSizeSeconds <= 0 {
		value.WindowSizeSeconds = 7200
	}
	now := time.Now().UTC()
	resetAt := now.Add(time.Duration(value.WindowSizeSeconds) * time.Second)
	return QuotaWindow{
		Mode: mode, Remaining: max(0, value.RemainingQueries), Total: value.TotalQueries,
		WindowSeconds: value.WindowSizeSeconds, ResetAt: &resetAt,
	}, nil
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
