package grok

import (
	"os"
	"strings"
	"time"
)

// ConfigFromEnv 读 Grok Web 出站与 Clearance 环境变量。
// 设了 FLARESOLVERR_URL / GROK_FLARESOLVERR_URL 且没写模式时，默认 on_demand：
// 账号已有 cf_clearance 就先用，遇到挑战或 Statsig 页面缺曲线再求解。
func ConfigFromEnv() Config {
	return Config{
		ProxyURL:           firstEnv("GROK_PROXY_URL", "PROXY_URL", "ALL_PROXY"),
		UserAgent:          strings.TrimSpace(os.Getenv("GROK_USER_AGENT")),
		StatsigMode:        strings.TrimSpace(os.Getenv("GROK_STATSIG_MODE")),
		StatsigManualValue: strings.TrimSpace(os.Getenv("GROK_STATSIG_MANUAL")),
		StatsigSignerURL:   firstEnv("GROK_STATSIG_SIGNER_URL", "STATSIG_SIGNER_URL"),
		StatsigCurvesFile:  strings.TrimSpace(os.Getenv("GROK_STATSIG_CURVES")),
		ClearanceMode:      strings.TrimSpace(os.Getenv("GROK_CLEARANCE_MODE")),
		FlareSolverrURL:    firstEnv("GROK_FLARESOLVERR_URL", "FLARESOLVERR_URL"),
		ClearanceTimeout:   parseEnvDuration("GROK_CLEARANCE_TIMEOUT"),
		ClearanceRefresh:   parseEnvDuration("GROK_CLEARANCE_REFRESH"),
	}
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func parseEnvDuration(key string) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return 0
	}
	if parsed, err := time.ParseDuration(value); err == nil {
		return parsed
	}
	return 0
}
