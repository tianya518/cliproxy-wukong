package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var (
	acceptTermsBody = []byte{0x00, 0x00, 0x00, 0x00, 0x02, 0x10, 0x01}
	enableNSFWBody  = []byte{
		0x00, 0x00, 0x00, 0x00, 0x20,
		0x0a, 0x02, 0x10, 0x01,
		0x12, 0x1a, 0x0a, 0x18,
		'a', 'l', 'w', 'a', 'y', 's', '_', 's', 'h', 'o', 'w', '_', 'n', 's', 'f', 'w', '_', 'c', 'o', 'n', 't', 'e', 'n', 't',
	}
)

func (c *Client) AcceptTerms(ctx context.Context) error {
	if err := c.postAccountBytes(ctx, officialAccountsBaseURL+"/auth_mgmt.AuthManagement/SetTosAcceptedVersion", acceptTermsBody, "application/grpc-web+proto", officialAccountsBaseURL, officialAccountsBaseURL+"/accept-tos", true, false); err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		TOSVersion int `json:"tosVersion"`
	}{TOSVersion: 1})
	if err != nil {
		return err
	}
	return c.postAccountBytes(ctx, c.baseURL()+"/rest/auth/set-tos-accepted", body, "application/json", c.baseURL(), c.baseURL()+"/", false, true)
}

func (c *Client) SetBirthDate(ctx context.Context, birthDate time.Time) error {
	data, err := json.Marshal(map[string]string{
		"birthDate": birthDate.UTC().Format("2006-01-02") + "T16:00:00.000Z",
	})
	if err != nil {
		return err
	}
	return c.postAccountBytes(ctx, c.baseURL()+"/rest/auth/set-birth-date", data, "application/json", c.baseURL(), c.baseURL()+"/", false, true)
}

func (c *Client) EnableNSFW(ctx context.Context) error {
	return c.postAccountBytes(ctx, c.baseURL()+"/auth_mgmt.AuthManagement/UpdateUserFeatureControls", enableNSFWBody, "application/grpc-web+proto", c.baseURL(), c.baseURL()+"/", true, true)
}

func (c *Client) postAccountBytes(ctx context.Context, endpoint string, body []byte, contentType, origin, referer string, grpcWeb, statsig bool) error {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	headers := c.restHeaders(contentType, referer)
	headers.Set("Origin", origin)
	if grpcWeb {
		headers.Set("x-grpc-web", "1")
		headers.Set("x-user-agent", "connect-es/2.1.1")
		headers.Del("x-xai-request-id")
	}
	if statsig {
		if err := c.applySignedStatsig(reqCtx, headers, http.MethodPost, endpoint); err != nil {
			return err
		}
	}
	resp, err := c.do(reqCtx, http.MethodPost, endpoint, body, headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, responseBodyLimit))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("账号设置 %s 返回 %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}
