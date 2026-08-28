package grok

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

type clearanceState struct {
	mu          sync.Mutex
	invalid     bool
	refreshedAt time.Time
}

func (c *Client) clearanceMode() string {
	mode := strings.ToLower(strings.TrimSpace(c.cfg.ClearanceMode))
	if mode != ClearanceModeFlareSolverr && mode != ClearanceModeOnDemand {
		return ClearanceModeManual
	}
	if strings.TrimSpace(c.cfg.FlareSolverrURL) == "" {
		return ClearanceModeManual
	}
	return mode
}

func (c *Client) clearanceEnabled() bool {
	return c.clearanceMode() != ClearanceModeManual
}

func (c *Client) cloudflareCookies() string {
	return strings.TrimSpace(c.cred.CloudflareCookies)
}

func (c *Client) invalidateClearance() {
	c.clearance.mu.Lock()
	c.clearance.invalid = true
	c.clearance.mu.Unlock()
}

func (c *Client) prepareMediaSession(ctx context.Context) {
	if c.clearanceMode() == ClearanceModeFlareSolverr {
		_ = c.ensureClearance(ctx, false)
	}
}

func (c *Client) ensureClearance(ctx context.Context, force bool) error {
	mode := c.clearanceMode()
	if mode == ClearanceModeManual {
		if force {
			return errors.New("未配置 FlareSolverr，无法刷新 Cloudflare Clearance")
		}
		return nil
	}
	c.clearance.mu.Lock()
	now := time.Now()
	interval := c.cfg.ClearanceRefresh
	if interval <= 0 {
		interval = defaultClearanceRefresh
	}
	if !force {
		if mode == ClearanceModeOnDemand && c.cloudflareCookies() != "" && !c.clearance.invalid {
			c.clearance.mu.Unlock()
			return nil
		}
		if mode == ClearanceModeFlareSolverr && c.cloudflareCookies() != "" && !c.clearance.invalid &&
			!c.clearance.refreshedAt.IsZero() && now.Sub(c.clearance.refreshedAt) < interval {
			c.clearance.mu.Unlock()
			return nil
		}
	}
	c.clearance.mu.Unlock()

	solution, err := solveFlareSolverr(ctx, c.cfg, c.cfg.ProxyURL)
	if err != nil {
		return err
	}

	c.clearance.mu.Lock()
	c.cred.CloudflareCookies = solution.Cookies
	if solution.UserAgent != "" {
		c.cred.UserAgent = solution.UserAgent
		c.cfg.UserAgent = solution.UserAgent
	}
	c.clearance.invalid = false
	c.clearance.refreshedAt = time.Now()
	hook := c.cfg.OnClearanceUpdate
	cred := c.cred
	c.clearance.mu.Unlock()
	if hook != nil {
		hook(cred)
	}
	return nil
}

func (c *Client) recoverMedia(ctx context.Context, err error, attempt int) bool {
	if attempt > 0 {
		return false
	}
	mediaErr, classified, ok := classifyMediaError(err)
	if !ok {
		return false
	}
	if isClearanceRefreshableMediaError(classified) {
		c.invalidateClearance()
		c.statsig.invalidate()
		if !c.clearanceEnabled() {
			return false
		}
		return c.ensureClearance(ctx, true) == nil
	}
	if isStatsigRefreshableMediaError(classified, mediaErr.body) {
		c.statsig.invalidate()
		if c.clearanceEnabled() {
			if c.ensureClearance(ctx, true) == nil {
				c.statsig.invalidate()
			}
		}
		return true
	}
	return false
}
