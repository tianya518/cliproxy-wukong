package grok

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// grok.com 设置页「使用量」不走 /rest/*，走同源 gRPC-web（application/grpc-web+proto）。
// 这里只手写 5 字节帧 + protowire 解码，不引 grpc 依赖。字段号来自前端 chunk 内嵌的
// FileDescriptorProto（prod/grok/backend/proto/grok_build_billing.proto、consumer_ui.proto）。
const (
	billingCreditsConfigPath   = "/grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig"
	billingRemainingResetsPath = "/prod_mc_billing.ConsumerUiSvc/GetRemainingResets"
	grpcWebBodyLimit           = 1 << 20
)

// billingProductNames 对应 billing_product.Product 枚举（前端 chunk 里的明文映射表）。
var billingProductNames = map[uint64]string{
	0: "unspecified",
	1: "api",
	2: "build",
	3: "plugins",
	4: "chat",
	5: "imagine",
	6: "voice",
	7: "app_builder",
	8: "tasks",
}

// BillingProductUsage 是当期内单个产品线占掉的额度百分比。
type BillingProductUsage struct {
	Product      string  `json:"product"`
	UsagePercent float64 `json:"usage_percent"`
}

// BillingResetToken 是「用量限额重置」券：validity_end 之前可以兑换一次，把当期用量清零。
type BillingResetToken struct {
	TokenID    string     `json:"token_id"`
	ValidFrom  *time.Time `json:"valid_from,omitempty"`
	ValidUntil *time.Time `json:"valid_until,omitempty"`
}

// BillingSnapshot 是订阅层面的额度：按周/月计的已用百分比、预付费余额、可用的重置券。
// 和 /rest/rate-limits 的 2 小时滚动窗口是两个维度。
type BillingSnapshot struct {
	UsagePercent        float64               `json:"usage_percent"`
	PeriodType          string                `json:"period_type"`
	PeriodStart         *time.Time            `json:"period_start,omitempty"`
	PeriodEnd           *time.Time            `json:"period_end,omitempty"`
	Products            []BillingProductUsage `json:"products,omitempty"`
	PrepaidBalanceCents int64                 `json:"prepaid_balance_cents"`
	OnDemandCapCents    int64                 `json:"on_demand_cap_cents"`
	OnDemandUsedCents   int64                 `json:"on_demand_used_cents"`
	UnifiedBilling      bool                  `json:"unified_billing"`
	Resets              []BillingResetToken   `json:"resets,omitempty"`
	ResetsError         string                `json:"resets_error,omitempty"`
}

// SyncBilling 拉订阅额度。GetGrokCreditsConfig 失败整体报错；重置券失败只记 ResetsError。
func (c *Client) SyncBilling(ctx context.Context) (BillingSnapshot, error) {
	raw, err := c.grpcWebUnary(ctx, billingCreditsConfigPath, nil)
	if err != nil {
		return BillingSnapshot{}, err
	}
	snapshot, err := parseCreditsConfigResponse(raw)
	if err != nil {
		return BillingSnapshot{}, err
	}
	if tokens, tokErr := c.grpcWebUnary(ctx, billingRemainingResetsPath, nil); tokErr != nil {
		snapshot.ResetsError = tokErr.Error()
	} else if resets, parseErr := parseRemainingResetsResponse(tokens); parseErr != nil {
		snapshot.ResetsError = parseErr.Error()
	} else {
		snapshot.Resets = resets
	}
	return snapshot, nil
}

// grpcWebUnary 发一次 gRPC-web unary 调用，返回响应消息的 protobuf 字节。
func (c *Client) grpcWebUnary(ctx context.Context, path string, message []byte) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.cfg.QuotaTimeout)
	defer cancel()
	headers := c.restHeaders("application/grpc-web+proto", c.baseURL()+"/imagine?_s=usage")
	headers.Set("Accept", "application/grpc-web+proto")
	headers.Set("X-Grpc-Web", "1")
	headers.Set("X-User-Agent", "grpc-web-javascript/0.1")

	frame := make([]byte, 5, 5+len(message))
	binary.BigEndian.PutUint32(frame[1:], uint32(len(message)))
	frame = append(frame, message...)

	resp, err := c.do(reqCtx, http.MethodPost, c.baseURL()+path, frame, headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, grpcWebBodyLimit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > grpcWebBodyLimit {
		return nil, fmt.Errorf("Grok 计费接口响应超过安全上限")
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, ErrUnauthorized
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, fmt.Errorf("Grok 计费接口 %s 返回 %d", path, resp.StatusCode)
	}
	// trailers-only：状态直接在 HTTP 头里（JSON 发错协议时就是这样回的）。
	if status := strings.TrimSpace(resp.Header.Get("Grpc-Status")); status != "" && status != "0" {
		return nil, fmt.Errorf("Grok 计费接口 gRPC 状态 %s: %s", status, decodeGrpcMessage(resp.Header.Get("Grpc-Message")))
	}
	return parseGrpcWebFrames(body)
}

// parseGrpcWebFrames 拆 gRPC-web 帧：0x00 数据帧拼起来返回，0x80 trailer 帧里检查 grpc-status。
func parseGrpcWebFrames(body []byte) ([]byte, error) {
	var message []byte
	sawData := false
	for len(body) > 0 {
		if len(body) < 5 {
			return nil, fmt.Errorf("Grok 计费接口响应帧头不完整")
		}
		flag := body[0]
		size := int(binary.BigEndian.Uint32(body[1:5]))
		if size > len(body)-5 {
			return nil, fmt.Errorf("Grok 计费接口响应帧长度越界")
		}
		payload := body[5 : 5+size]
		body = body[5+size:]
		if flag&0x80 != 0 {
			for _, line := range strings.Split(string(payload), "\r\n") {
				key, value, ok := strings.Cut(line, ":")
				if !ok || !strings.EqualFold(strings.TrimSpace(key), "grpc-status") {
					continue
				}
				if status := strings.TrimSpace(value); status != "0" {
					return nil, fmt.Errorf("Grok 计费接口 gRPC 状态 %s: %s", status, decodeGrpcMessage(grpcTrailerValue(payload, "grpc-message")))
				}
			}
			continue
		}
		sawData = true
		message = append(message, payload...)
	}
	if !sawData {
		return nil, fmt.Errorf("Grok 计费接口没有返回数据帧")
	}
	return message, nil
}

func grpcTrailerValue(payload []byte, name string) string {
	for _, line := range strings.Split(string(payload), "\r\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func decodeGrpcMessage(value string) string {
	value = strings.TrimSpace(value)
	if decoded, err := url.PathUnescape(value); err == nil {
		return decoded
	}
	return value
}

// wireField 是一条已消费的字段；嵌套消息按 bytes 原样保留，由调用方按字段号再解。
type wireField struct {
	num   protowire.Number
	typ   protowire.Type
	uint  uint64
	bytes []byte
}

func walkWire(raw []byte, visit func(wireField) error) error {
	for len(raw) > 0 {
		num, typ, n := protowire.ConsumeTag(raw)
		if n < 0 {
			return protowire.ParseError(n)
		}
		raw = raw[n:]
		field := wireField{num: num, typ: typ}
		switch typ {
		case protowire.VarintType:
			v, m := protowire.ConsumeVarint(raw)
			if m < 0 {
				return protowire.ParseError(m)
			}
			field.uint, n = v, m
		case protowire.Fixed32Type:
			v, m := protowire.ConsumeFixed32(raw)
			if m < 0 {
				return protowire.ParseError(m)
			}
			field.uint, n = uint64(v), m
		case protowire.Fixed64Type:
			v, m := protowire.ConsumeFixed64(raw)
			if m < 0 {
				return protowire.ParseError(m)
			}
			field.uint, n = v, m
		case protowire.BytesType:
			v, m := protowire.ConsumeBytes(raw)
			if m < 0 {
				return protowire.ParseError(m)
			}
			field.bytes, n = v, m
		default:
			n = protowire.ConsumeFieldValue(num, typ, raw)
			if n < 0 {
				return protowire.ParseError(n)
			}
		}
		raw = raw[n:]
		if err := visit(field); err != nil {
			return err
		}
	}
	return nil
}

// parseCent 解 prod_charger.Cent{int64 val = 1}。
func parseCent(raw []byte) int64 {
	var cents int64
	_ = walkWire(raw, func(f wireField) error {
		if f.num == 1 && f.typ == protowire.VarintType {
			cents = int64(f.uint)
		}
		return nil
	})
	return cents
}

// parseTimestamp 解 google.protobuf.Timestamp{int64 seconds = 1; int32 nanos = 2}。
func parseTimestamp(raw []byte) *time.Time {
	var seconds, nanos int64
	seen := false
	_ = walkWire(raw, func(f wireField) error {
		if f.typ != protowire.VarintType {
			return nil
		}
		switch f.num {
		case 1:
			seconds, seen = int64(f.uint), true
		case 2:
			nanos, seen = int64(f.uint), true
		}
		return nil
	})
	if !seen {
		return nil
	}
	value := time.Unix(seconds, nanos).UTC()
	return &value
}

func billingPeriodTypeName(value uint64) string {
	switch value {
	case 1:
		return "monthly"
	case 2:
		return "weekly"
	default:
		return "unspecified"
	}
}

func billingProductName(value uint64) string {
	if name, ok := billingProductNames[value]; ok {
		return name
	}
	return fmt.Sprintf("product_%d", value)
}

// parseCreditsConfigResponse 解 GetGrokCreditsConfigResponse{GrokCreditsConfig config = 1}。
func parseCreditsConfigResponse(raw []byte) (BillingSnapshot, error) {
	var config []byte
	if err := walkWire(raw, func(f wireField) error {
		if f.num == 1 && f.typ == protowire.BytesType {
			config = f.bytes
		}
		return nil
	}); err != nil {
		return BillingSnapshot{}, fmt.Errorf("解析 Grok 计费响应: %w", err)
	}
	if config == nil {
		return BillingSnapshot{}, fmt.Errorf("Grok 计费响应缺少 config")
	}
	snapshot := BillingSnapshot{PeriodType: "unspecified"}
	err := walkWire(config, func(f wireField) error {
		switch f.num {
		case 1: // float credit_usage_percent
			if f.typ == protowire.Fixed32Type {
				snapshot.UsagePercent = roundPercent(float64(math.Float32frombits(uint32(f.uint))))
			}
		case 2: // Cent on_demand_cap
			snapshot.OnDemandCapCents = parseCent(f.bytes)
		case 3: // Cent on_demand_used
			snapshot.OnDemandUsedCents = parseCent(f.bytes)
		case 4: // Timestamp billing_period_start
			snapshot.PeriodStart = parseTimestamp(f.bytes)
		case 5: // Timestamp billing_period_end
			snapshot.PeriodEnd = parseTimestamp(f.bytes)
		case 7: // repeated ProductUsage{Product product = 1; float usage_percent = 2}
			usage := BillingProductUsage{Product: billingProductName(0)}
			_ = walkWire(f.bytes, func(p wireField) error {
				switch {
				case p.num == 1 && p.typ == protowire.VarintType:
					usage.Product = billingProductName(p.uint)
				case p.num == 2 && p.typ == protowire.Fixed32Type:
					usage.UsagePercent = roundPercent(float64(math.Float32frombits(uint32(p.uint))))
				}
				return nil
			})
			snapshot.Products = append(snapshot.Products, usage)
		case 8: // UsagePeriod current_period{type = 1; start = 2; end = 3}
			_ = walkWire(f.bytes, func(p wireField) error {
				switch {
				case p.num == 1 && p.typ == protowire.VarintType:
					snapshot.PeriodType = billingPeriodTypeName(p.uint)
				case p.num == 2 && p.typ == protowire.BytesType && snapshot.PeriodStart == nil:
					snapshot.PeriodStart = parseTimestamp(p.bytes)
				case p.num == 3 && p.typ == protowire.BytesType && snapshot.PeriodEnd == nil:
					snapshot.PeriodEnd = parseTimestamp(p.bytes)
				}
				return nil
			})
		case 11: // bool is_unified_billing_user
			snapshot.UnifiedBilling = f.typ == protowire.VarintType && f.uint != 0
		case 12: // Cent prepaid_balance
			snapshot.PrepaidBalanceCents = parseCent(f.bytes)
		}
		return nil
	})
	if err != nil {
		return BillingSnapshot{}, fmt.Errorf("解析 Grok 计费配置: %w", err)
	}
	return snapshot, nil
}

// parseRemainingResetsResponse 解 ConsumerGetRemainingResetsResp{repeated ConsumerResetToken tokens = 10}，
// ConsumerResetToken{string token_id = 10; Timestamp validity_start = 20; Timestamp validity_end = 30}。
func parseRemainingResetsResponse(raw []byte) ([]BillingResetToken, error) {
	var tokens []BillingResetToken
	err := walkWire(raw, func(f wireField) error {
		if f.num != 10 || f.typ != protowire.BytesType {
			return nil
		}
		token := BillingResetToken{}
		_ = walkWire(f.bytes, func(p wireField) error {
			switch {
			case p.num == 10 && p.typ == protowire.BytesType:
				token.TokenID = string(p.bytes)
			case p.num == 20 && p.typ == protowire.BytesType:
				token.ValidFrom = parseTimestamp(p.bytes)
			case p.num == 30 && p.typ == protowire.BytesType:
				token.ValidUntil = parseTimestamp(p.bytes)
			}
			return nil
		})
		if token.TokenID != "" {
			tokens = append(tokens, token)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("解析 Grok 重置券: %w", err)
	}
	return tokens, nil
}

func roundPercent(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0
	}
	return math.Round(value*100) / 100
}
