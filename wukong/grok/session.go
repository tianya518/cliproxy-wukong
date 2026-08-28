package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var gatewayUserIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Identity struct {
	UserID string
	Email  string
	TeamID string
}

func (c *Client) FetchSession(ctx context.Context) (Identity, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := c.do(reqCtx, http.MethodGet, c.baseURL()+"/api/auth/session", nil, c.sessionHeaders())
	if err != nil {
		return Identity{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, responseBodyLimit+1))
	if err != nil {
		return Identity{}, err
	}
	body, err = decodeWireBody(body)
	if err != nil {
		return Identity{}, err
	}
	if len(body) > responseBodyLimit {
		return Identity{}, fmt.Errorf("Grok Session 响应超过安全上限")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return Identity{}, ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Identity{}, fmt.Errorf("Grok Session 接口返回 %d", resp.StatusCode)
	}
	identity, err := parseSession(body)
	if err != nil {
		return Identity{}, err
	}
	if identity.UserID != "" {
		c.resolvedUserID = identity.UserID
		c.cred.UserID = identity.UserID
	}
	if identity.Email != "" && c.cred.Email == "" {
		c.cred.Email = identity.Email
	}
	return identity, nil
}

func parseSession(body []byte) (Identity, error) {
	var value struct {
		Status  string `json:"status"`
		Session struct {
			UserID         string `json:"userId"`
			Email          string `json:"email"`
			OrganizationID string `json:"organizationId"`
		} `json:"session"`
		User struct {
			ID     string `json:"id"`
			UserID string `json:"userId"`
			Sub    string `json:"sub"`
			Email  string `json:"email"`
			TeamID string `json:"teamId"`
		} `json:"user"`
		ID     string `json:"id"`
		UserID string `json:"userId"`
		Sub    string `json:"sub"`
		Email  string `json:"email"`
		TeamID string `json:"teamId"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return Identity{}, fmt.Errorf("解析 Grok Session: %w", err)
	}
	status := strings.TrimSpace(value.Status)
	if strings.EqualFold(status, "unauthenticated") {
		return Identity{}, ErrUnauthorized
	}
	if strings.EqualFold(status, "blocked") {
		return Identity{}, fmt.Errorf("%w: session status blocked", ErrUnauthorized)
	}
	identity := Identity{
		UserID: firstNonEmpty(value.Session.UserID, value.User.ID, value.User.UserID, value.User.Sub, value.ID, value.UserID, value.Sub),
		Email:  firstNonEmpty(value.Session.Email, value.User.Email, value.Email),
		TeamID: firstNonEmpty(value.Session.OrganizationID, value.User.TeamID, value.TeamID),
	}
	if identity.UserID == "" && identity.Email == "" {
		return Identity{}, fmt.Errorf("Grok Session 缺少账号身份")
	}
	return identity, nil
}

func (c *Client) gatewayUserID(ctx context.Context) (string, error) {
	if id, err := normalizeGatewayUserID(firstNonEmpty(c.resolvedUserID, c.cred.UserID)); err == nil {
		c.resolvedUserID = id
		return id, nil
	}
	identity, err := c.FetchSession(ctx)
	if err != nil {
		return "", fmt.Errorf("同步 Grok Web Gateway 用户身份: %w", err)
	}
	return normalizeGatewayUserID(identity.UserID)
}

func normalizeGatewayUserID(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if !gatewayUserIDPattern.MatchString(value) {
		return "", fmt.Errorf("Grok Web 账号缺少有效 user_id，请先同步账号资料")
	}
	return value, nil
}
