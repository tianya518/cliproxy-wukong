package server

import (
	"io"
	"net/http"
	"time"
)

// meProbeURL 用于在仅有 Access Token 时做一次轻量存活探测。
const meProbeURL = "https://chatgpt.com/backend-api/me"

var probeHTTPClient = &http.Client{Timeout: 20 * time.Second}

// TokenCheckResult 单条凭证的检测结果。
type TokenCheckResult struct {
	ID         string    `json:"id"`
	HasAccess  bool      `json:"has_access_token"`
	HasSession bool      `json:"has_session_token"`
	Valid      bool      `json:"valid"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
	Refreshed  bool      `json:"refreshed,omitempty"`
	Note       string    `json:"note,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// CheckAll 主动检测池内每条凭证是否可用：
//   - Access Token 未过期：直接视为有效，不去 chatgpt.com 换票。
//   - AT 过期或缺失：再用 refresh_token / session token 换新 AT。
//
// 检测结果会同步更新 errorKeys（有效则解封，失效则标记），并持久化刷新后的 AT。
func (tp *TokenPool) CheckAll() []TokenCheckResult {
	tp.mu.Lock()
	snapshot := make([]storedToken, len(tp.entries))
	copy(snapshot, tp.entries)
	tp.mu.Unlock()

	results := make([]TokenCheckResult, 0, len(snapshot))
	refreshed := make(map[string]storedToken) // dedupKey -> 刷新后的凭证
	validity := make(map[string]bool)         // dedupKey -> 是否有效

	for i := range snapshot {
		e := snapshot[i]
		e.assignID()
		key := e.dedupKey()
		r := TokenCheckResult{
			ID:         e.ID,
			HasAccess:  e.AccessToken != "",
			HasSession: e.SessionToken != "",
		}

		result, updated := CheckChatGPTCredential(Credential{
			ID:           e.ID,
			AccessToken:  e.AccessToken,
			SessionToken: e.SessionToken,
			RefreshToken: e.RefreshToken,
			ExpiresAt:    e.ExpiresAt,
		})
		r = result
		if result.Valid && result.Refreshed {
			e.AccessToken = updated.AccessToken
			e.RefreshToken = updated.RefreshToken
			e.SessionToken = updated.SessionToken
			e.ExpiresAt = updated.ExpiresAt
			e.UpdatedAt = time.Now()
			refreshed[key] = e
		}

		if key != "" {
			validity[key] = r.Valid
		}
		results = append(results, r)
	}

	tp.mu.Lock()
	for i := range tp.entries {
		key := tp.entries[i].dedupKey()
		if upd, ok := refreshed[key]; ok {
			tp.entries[i].AccessToken = upd.AccessToken
			tp.entries[i].ExpiresAt = upd.ExpiresAt
			tp.entries[i].UpdatedAt = upd.UpdatedAt
		}
		if v, ok := validity[key]; ok {
			if v {
				delete(tp.errorKeys, key)
			} else {
				tp.errorKeys[key] = true
			}
		}
	}
	tp.persistLocked()
	tp.mu.Unlock()

	return results
}

// CheckChatGPTCredential probes one credential. It does not persist.
// A still-valid access token from /api/auth/session is used as-is.
// Session / OAuth refresh only runs when the AT is missing or expired.
func CheckChatGPTCredential(cred Credential) (TokenCheckResult, Credential) {
	r := TokenCheckResult{
		ID:         cred.ID,
		HasAccess:  cred.AccessToken != "",
		HasSession: cred.SessionToken != "",
	}
	if exp, ok := AccessTokenFresh(cred.AccessToken, time.Now()); ok {
		r.Valid = true
		r.HasAccess = true
		r.ExpiresAt = exp
		cred.ExpiresAt = exp
		return r, cred
	}

	switch {
	case cred.RefreshToken != "":
		at, newRT, exp, err := RefreshATFromRefreshToken(cred.RefreshToken, "", "")
		if err != nil {
			r.Valid = false
			r.Error = err.Error()
			return r, cred
		}
		r.Valid = true
		r.Refreshed = true
		r.HasAccess = true
		r.ExpiresAt = exp
		cred.AccessToken = at
		cred.ExpiresAt = exp
		if newRT != "" {
			cred.RefreshToken = newRT
		}
	case cred.SessionToken != "":
		at, exp, err := RefreshATFromSession(cred.SessionToken)
		if err != nil {
			r.Valid = false
			r.Error = err.Error()
			return r, cred
		}
		r.Valid = true
		r.Refreshed = true
		r.HasAccess = true
		r.ExpiresAt = exp
		cred.AccessToken = at
		cred.ExpiresAt = exp
	case cred.AccessToken != "":
		exp := parseJWTExp(cred.AccessToken)
		r.ExpiresAt = exp
		r.Valid = false
		r.Error = "access token expired"
	default:
		r.Valid = false
		r.Error = "empty entry"
	}
	return r, cred
}

// probeAccessToken 用 Access Token 轻量探测存活。
// 200 → 有效；401 → 失效；其它（含 Cloudflare 拦截）→ 无法确认，按有效处理并附注说明。
func probeAccessToken(accessToken string) (bool, string) {
	req, err := http.NewRequest(http.MethodGet, meProbeURL, nil)
	if err != nil {
		return true, "probe build error: " + err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")

	resp, err := probeHTTPClient.Do(req)
	if err != nil {
		return true, "probe request failed (assumed valid): " + err.Error()
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
		return true, ""
	case http.StatusUnauthorized, http.StatusForbidden:
		if resp.StatusCode == http.StatusForbidden {
			return true, "probe http=403 (可能被 Cloudflare 拦截，按有效处理)"
		}
		return false, "unauthorized"
	default:
		return true, "probe http=" + http.StatusText(resp.StatusCode) + " (无法确认，按有效处理)"
	}
}
