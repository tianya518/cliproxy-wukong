package grok

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrAntiBot      = errors.New("Grok Web anti-bot rejection")
	ErrUsageLimit   = errors.New("Grok Web usage limit reached")
	ErrNoInput      = errors.New("no user message or attachments found in messages")
	ErrUnauthorized = errors.New("Grok Web unauthorized")
)

// GatewayStatusError 是 Grok WS 在 response.done 里给出的非 completed 状态。
type GatewayStatusError struct {
	Status string
}

func (e *GatewayStatusError) Error() string {
	if e == nil {
		return "Grok Gateway response 状态未知"
	}
	return "Grok Gateway response 状态为 " + e.Status
}

func (e *GatewayStatusError) Soft() bool {
	if e == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(e.Status)) {
	case "incomplete", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

const (
	maxChatAttachments            = 8
	maxChatAttachmentTotal        = 64 << 20
	maxRemoteAttachmentURLLen     = 8192
	maxDeferredSearchTextBytes    = 8 << 20
	maxTrackedServerTools         = 1024
	maxTrackedCitationSources     = 256
	maxTrackedAnnotations         = 2048
	maxGeneratedImages            = 10
	maxFunctionTools              = 128
	maxToolDescriptionSize        = 16 << 10
	imagineSelfUploadSource       = "IMAGINE_SELF_UPLOAD_FILE_SOURCE"
	directFileUploadResponseLimit = 2 << 20
	webMediaDiagnosticBodyLimit   = 64 << 10
	responseBodyLimit             = 4 << 20
	officialAccountsBaseURL       = "https://accounts.x.ai"
)

type Config struct {
	BaseURL            string
	ProxyURL           string
	UserAgent          string
	AllowNSFW          bool
	StatsigMode        string
	StatsigManualValue string
	StatsigSignerURL   string
	StatsigCurvesFile  string
	ClearanceMode      string
	FlareSolverrURL    string
	ClearanceTimeout   time.Duration
	ClearanceRefresh   time.Duration
	OnClearanceUpdate  func(Credential)
	ChatTimeout        time.Duration
	ImageTimeout       time.Duration
	VideoTimeout       time.Duration
	QuotaTimeout       time.Duration
	MaxInputImageBytes int64
}

func (c Config) normalized() Config {
	if strings.TrimSpace(c.BaseURL) == "" {
		c.BaseURL = DefaultBaseURL
	}
	if strings.TrimSpace(c.UserAgent) == "" {
		c.UserAgent = DefaultUserAgent
	}
	if c.ChatTimeout <= 0 {
		c.ChatTimeout = 120 * time.Second
	}
	if c.ImageTimeout <= 0 {
		c.ImageTimeout = 180 * time.Second
	}
	if c.VideoTimeout <= 0 {
		c.VideoTimeout = 900 * time.Second
	}
	if c.QuotaTimeout <= 0 {
		c.QuotaTimeout = 25 * time.Second
	}
	if c.MaxInputImageBytes <= 0 {
		c.MaxInputImageBytes = 32 << 20
	}
	mode := strings.ToLower(strings.TrimSpace(c.StatsigMode))
	switch mode {
	case StatsigModeManual, StatsigModeURL, StatsigModeLocal:
		c.StatsigMode = mode
	default:
		c.StatsigMode = StatsigModeLocal
	}
	if c.ClearanceTimeout <= 0 {
		c.ClearanceTimeout = defaultClearanceTimeout
	}
	if c.ClearanceRefresh <= 0 {
		c.ClearanceRefresh = defaultClearanceRefresh
	}
	clearanceMode := strings.ToLower(strings.TrimSpace(c.ClearanceMode))
	switch clearanceMode {
	case ClearanceModeFlareSolverr, ClearanceModeOnDemand, ClearanceModeManual:
		c.ClearanceMode = clearanceMode
	case "":
		if strings.TrimSpace(c.FlareSolverrURL) != "" {
			c.ClearanceMode = ClearanceModeOnDemand
		} else {
			c.ClearanceMode = ClearanceModeManual
		}
	default:
		c.ClearanceMode = ClearanceModeManual
	}
	return c
}

type TurnState struct {
	ConversationID string
	ParentID       string
}

type hostedSearchCall struct {
	ID      string
	Kind    string
	Query   string
	Status  string
	Sources []map[string]any
}

type trackedTextBuilder struct {
	builder    strings.Builder
	characters int
}

func (b *trackedTextBuilder) WriteString(value string) (int, error) {
	written, err := b.builder.WriteString(value)
	b.characters += utf8.RuneCountInString(value[:written])
	return written, err
}

func (b *trackedTextBuilder) Reset() {
	b.builder.Reset()
	b.characters = 0
}

func (b *trackedTextBuilder) String() string { return b.builder.String() }

func (b *trackedTextBuilder) Len() int { return b.builder.Len() }

func (b *trackedTextBuilder) CharacterLen() int { return b.characters }

type parsedChat struct {
	ResponseID             string
	ConversationID         string
	ParentID               string
	Text                   trackedTextBuilder
	upstreamText           strings.Builder
	Reasoning              strings.Builder
	Images                 []string
	SearchSources          []map[string]any
	Annotations            []map[string]any
	HostedSearchCalls      []hostedSearchCall
	hostedSearchByID       map[string]int
	DisableInlineCitations bool
	sourceKeys             map[string]struct{}
	serverToolKeys         map[string]struct{}
	webSearchKeys          map[string]struct{}
	xSearchKeys            map[string]struct{}
	cardCache              map[string]map[string]any
	moderatedImages        map[string]struct{}
	citationIndex          map[string]int
	lastCitation           int
	ServerTools            int64
	WebSearchTools         int64
	XSearchTools           int64
	ToolCalls              []parsedToolCall
}

func (p *parsedChat) hasVisibleOutput() bool {
	if p == nil {
		return false
	}
	return strings.TrimSpace(p.Text.String()) != "" || len(p.Images) > 0 || len(p.ToolCalls) > 0
}

func (p *parsedChat) textCharacterLen() int {
	if p == nil {
		return 0
	}
	return p.Text.CharacterLen()
}

func (p *parsedChat) appendText(value string) {
	if p == nil || value == "" {
		return
	}
	p.Text.WriteString(value)
}

type chatAttachmentInput struct {
	Source   string
	Filename string
	Image    bool
}

type normalizedChatInput struct {
	Prompt      string
	Attachments []chatAttachmentInput
}

type uploadedFile struct {
	ID         string
	MetadataID string
	URI        string
}

type fileBytes struct {
	Filename string
	MIMEType string
	Data     []byte
}
