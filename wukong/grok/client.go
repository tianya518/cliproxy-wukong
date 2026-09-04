package grok

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/imroc/req/v3"
)

type Client struct {
	cfg              Config
	cred             Credential
	http             *req.Client
	statsig          *statsigSigner
	clearance        clearanceState
	conversationID   string
	parentResponseID string
	resolvedUserID   string
}

func NewClient(cfg Config, cred Credential) *Client {
	cfg = cfg.normalized()
	c := &Client{cfg: cfg, cred: cred}
	c.statsig = sharedStatsigSigner(c.baseURL())
	httpC := req.C().
		SetBaseURL(c.baseURL()).
		ImpersonateChrome().
		SetTLSFingerprintChrome().
		EnableAutoDecode().
		SetTimeout(cfg.ChatTimeout)
	httpC.SetTLSHandshakeTimeout(20 * time.Second)
	if os.Getenv("GROK_ALLOW_IPV6") == "" {
		dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
		httpC.SetDial(func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		})
	}
	if proxy := strings.TrimSpace(cfg.ProxyURL); proxy != "" {
		httpC.SetProxyURL(proxy)
	}
	c.http = httpC
	return c
}

func (c *Client) Credential() Credential { return c.cred }

func (c *Client) SetConversation(conversationID, parentID string) {
	c.conversationID = strings.TrimSpace(conversationID)
	c.parentResponseID = strings.TrimSpace(parentID)
}

func (c *Client) Conversation() TurnState {
	return TurnState{ConversationID: c.conversationID, ParentID: c.parentResponseID}
}

func (c *Client) do(ctx context.Context, method, endpoint string, body []byte, headers http.Header) (*http.Response, error) {
	r := c.http.R().SetContext(ctx)
	for key, values := range headers {
		if len(values) == 0 {
			continue
		}
		r.SetHeader(key, values[0])
	}
	if body != nil {
		r.SetBody(body)
	}
	resp, err := r.Send(method, endpoint)
	if err != nil {
		return nil, err
	}
	httpResp := resp.Response
	if httpResp == nil {
		return nil, io.ErrUnexpectedEOF
	}
	// 必须用 req 解压后的字节。直接读 http.Response.Body 会拿到还没解的 gzip（\x1f）。
	raw, err := decodeWireBody(resp.Bytes())
	if err != nil {
		return nil, err
	}
	httpResp.Body = io.NopCloser(bytes.NewReader(raw))
	httpResp.Header.Del("Content-Encoding")
	httpResp.ContentLength = int64(len(raw))
	return httpResp, nil
}

func (c *Client) doJSON(ctx context.Context, endpoint string, payload []byte, referer string, timeout time.Duration, withStatsig bool) (*http.Response, error) {
	if timeout <= 0 {
		timeout = c.cfg.ChatTimeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	headers := c.restHeaders("application/json", referer)
	if withStatsig {
		if err := c.applySignedStatsig(reqCtx, headers, http.MethodPost, endpoint); err != nil {
			return nil, err
		}
	}
	return c.do(reqCtx, http.MethodPost, endpoint, payload, headers)
}

func (c *Client) dialWS(ctx context.Context, endpoint string, extra http.Header) (*websocket.Conn, *http.Response, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 20 * time.Second,
		Proxy:            http.ProxyFromEnvironment,
	}
	if proxy := strings.TrimSpace(c.cfg.ProxyURL); proxy != "" {
		if parsed, err := url.Parse(proxy); err == nil {
			dialer.Proxy = http.ProxyURL(parsed)
		}
	}
	header := http.Header{}
	for key, values := range extra {
		for _, value := range values {
			header.Add(key, value)
		}
	}
	return dialer.DialContext(ctx, endpoint, header)
}
