package grok

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestGatewayEndpointAndHeadersMatchBrowserProtocol(t *testing.T) {
	const userID = "497f19f8-49d4-458a-bee4-43ec3dcaf8ca"
	endpoint, origin, err := gatewayEndpoint("https://grok.com", userID)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "wss://grok.com/ws/mgw/?uid="+userID || origin != "https://grok.com" {
		t.Fatalf("endpoint=%q origin=%q", endpoint, origin)
	}
	headers := gatewayHeaders(origin, userID, "test-sso", "cf_clearance=test-cf", "test-agent")
	cookie := headers.Get("Cookie")
	for _, expected := range []string{"sso=test-sso", "sso-rw=test-sso", "x-userid=" + userID, "cf_clearance=test-cf"} {
		if !strings.Contains(cookie, expected) {
			t.Fatalf("cookie %q missing %q", cookie, expected)
		}
	}
	if headers.Get("Authorization") != "" {
		t.Fatalf("unexpected authorization headers: %#v", headers)
	}
}

func TestGatewaySessionSupportsNewAndExistingConversations(t *testing.T) {
	fresh := gatewaySession("fast", nil)
	freshXGrok := fresh["x_grok"].(map[string]any)
	if fresh["model"] != "fast" || freshXGrok["is_temporary"] != true || freshXGrok["load_existing"] != nil {
		t.Fatalf("fresh session = %#v", fresh)
	}
	previous := &TurnState{ConversationID: "conversation-1", ParentID: "response-1"}
	existing := gatewaySession("expert", previous)
	existingXGrok := existing["x_grok"].(map[string]any)
	if existing["model"] != "expert" || existingXGrok["conversation_id"] != "conversation-1" || existingXGrok["load_existing"] != true || existingXGrok["needs_history"] != false {
		t.Fatalf("existing session = %#v", existing)
	}
}

func TestGatewayTurnEventsOmitCastleAndPreserveAttachments(t *testing.T) {
	previous := &TurnState{ParentID: "response-1"}
	item, response := gatewayTurnEvents("conversation-1", "hello", []string{"file-1"}, previous)
	itemEvent := item["event"].(map[string]any)
	if item["session_id"] != "conversation-1" || itemEvent["parent_response_id"] != "response-1" {
		t.Fatalf("item event = %#v", item)
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, expected := range []string{`"file_attachment_ids":["file-1"]`, `"file_mention":{"file_id":"file-1"}`, `"text":{"text":"hello"}`} {
		if !strings.Contains(text, expected) {
			t.Fatalf("item JSON %s missing %s", text, expected)
		}
	}
	responseJSON, _ := json.Marshal(response)
	if strings.Contains(string(responseJSON), "castle_request_token") {
		t.Fatalf("response.create unexpectedly contains Castle token: %s", responseJSON)
	}
}

func TestParseGatewayEventsCollectsConversationTextAndParent(t *testing.T) {
	parsed := &parsedChat{}
	frames := []string{
		`{"event":{"type":"conversation.attached","conversation":{"id":"conversation-1"}}}`,
		`{"event":{"type":"response.chunk","chunk":{"text":{"text":"TOKEN","channel":"CHANNEL_ASSISTANT_RESPONSE"}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"text":{"text":"LESS","channel":"CHANNEL_ASSISTANT_RESPONSE"}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"text":{"text":"thought","channel":"CHANNEL_ANALYSIS"}}}}`,
		`{"event":{"type":"response.done","response":{"id":"response-1","status":"completed"}}}`,
	}
	var emitted strings.Builder
	for _, frame := range frames {
		kind, delta, err := parseUpstreamFrame([]byte(frame), parsed)
		if err != nil {
			t.Fatal(err)
		}
		if kind == "text" {
			emitted.WriteString(delta)
		}
	}
	if parsed.ConversationID != "conversation-1" || parsed.ParentID != "response-1" || parsed.Text.String() != "TOKENLESS" || emitted.String() != "TOKENLESS" || parsed.Reasoning.String() != "thought" {
		t.Fatalf("parsed = conversation=%q parent=%q text=%q emitted=%q reasoning=%q", parsed.ConversationID, parsed.ParentID, parsed.Text.String(), emitted.String(), parsed.Reasoning.String())
	}
}

func TestParseGatewayIncompleteWithTextSucceeds(t *testing.T) {
	parsed := &parsedChat{}
	frames := []string{
		`{"event":{"type":"response.chunk","chunk":{"text":{"text":"HELLO","channel":"CHANNEL_ASSISTANT_RESPONSE"}}}}`,
		`{"event":{"type":"response.done","response":{"id":"response-1","status":"incomplete"}}}`,
	}
	for _, frame := range frames {
		if _, _, err := parseUpstreamFrame([]byte(frame), parsed); err != nil {
			t.Fatal(err)
		}
	}
	if parsed.Text.String() != "HELLO" || parsed.ParentID != "response-1" {
		t.Fatalf("text=%q parent=%q", parsed.Text.String(), parsed.ParentID)
	}
}

func TestParseGatewayIncompleteWithoutTextErrors(t *testing.T) {
	_, _, err := parseUpstreamFrame([]byte(`{"event":{"type":"response.done","response":{"id":"response-1","status":"incomplete"}}}`), &parsedChat{})
	var statusErr *GatewayStatusError
	if !errors.As(err, &statusErr) || !statusErr.Soft() || statusErr.Status != "incomplete" {
		t.Fatalf("error = %v", err)
	}
}

func TestParseGatewayErrorUsesExistingClassification(t *testing.T) {
	_, _, err := parseUpstreamFrame([]byte(`{"event":{"type":"error","error":{"code":"anti_bot","message":"anti-bot rejected"}}}`), &parsedChat{})
	if !errors.Is(err, ErrAntiBot) {
		t.Fatalf("error = %v", err)
	}
}

func TestParseGatewayChunkCollectsToolResultsAndRenderCitations(t *testing.T) {
	parsed := &parsedChat{}
	frames := []string{
		`{"event":{"type":"response.chunk","chunk":{"tool_usage_card":{"tool_usage_card_id":"tool-1","web_search":{"args":{"query":"grok 4.6"}}}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"tool_result":{"tool_call_id":"tool-1","web_search":{"webpages":[{"url":"https://www.ithome.com/0/981/947.htm","title":"IT之家 Grok 4.6"}]}}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"tool_usage_card":{"tool_usage_card_id":"tool-2","x_search":{"args":{"query":"from:elonmusk"}}}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"tool_result":{"tool_call_id":"tool-2","x_post":{"posts":[{"userhandle":"elonmusk","name":"Elon Musk","text":"And Grok 4.6 comes out in a week","post_id":"2082707547203518569"}]}}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"text":{"text":"预计发布","channel":"CHANNEL_ASSISTANT_RESPONSE"}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"render_citation":{"id":"c1","kind":"CITATION_KIND_X_POST","url":"https://x.com/elonmusk/status/2082707547203518569","citation_id":1}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"render_citation":{"id":"c2","kind":"CITATION_KIND_WEB_PAGE","url":"https://www.ithome.com/0/981/947.htm","citation_id":0}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"text":{"text":"。","channel":"CHANNEL_ASSISTANT_RESPONSE"}}}}`,
	}
	var emitted strings.Builder
	for _, frame := range frames {
		kind, delta, err := parseUpstreamFrame([]byte(frame), parsed)
		if err != nil {
			t.Fatal(err)
		}
		if kind == "text" {
			emitted.WriteString(delta)
		}
	}
	if parsed.ServerTools != 2 || parsed.WebSearchTools != 1 {
		t.Fatalf("tools server=%d web=%d", parsed.ServerTools, parsed.WebSearchTools)
	}
	if len(parsed.SearchSources) < 2 {
		t.Fatalf("search sources = %#v", parsed.SearchSources)
	}
	if len(parsed.HostedSearchCalls) < 2 {
		t.Fatalf("hosted search calls = %#v", parsed.HostedSearchCalls)
	}
	if parsed.HostedSearchCalls[0].Kind != "web_search" || parsed.HostedSearchCalls[0].Status != "completed" {
		t.Fatalf("web call = %#v", parsed.HostedSearchCalls[0])
	}
	if parsed.HostedSearchCalls[1].Kind != "x_search" || parsed.HostedSearchCalls[1].Status != "completed" {
		t.Fatalf("x call = %#v", parsed.HostedSearchCalls[1])
	}
	if len(parsed.Annotations) != 2 {
		t.Fatalf("annotations = %#v", parsed.Annotations)
	}
	text := parsed.Text.String()
	if !strings.Contains(text, "预计发布") || !strings.Contains(text, "[[1]](https://x.com/elonmusk/status/2082707547203518569)") || !strings.Contains(text, "[[2]](https://www.ithome.com/0/981/947.htm)") {
		t.Fatalf("text = %q emitted = %q", text, emitted.String())
	}
}

func TestImagineAndVideoPayloadsMatchUpstream(t *testing.T) {
	reset, _ := json.Marshal(imagineResetMessage())
	if !strings.Contains(string(reset), `"type":"reset"`) {
		t.Fatalf("reset = %s", reset)
	}
	req, _ := json.Marshal(imagineRequestMessage("img_1", "a cat", "16:9", true, true, 2))
	for _, want := range []string{`"aspect_ratio":"16:9"`, `"enable_pro":true`, `"num_generations":2`, `"enable_nsfw":true`} {
		if !strings.Contains(string(req), want) {
			t.Fatalf("imagine request missing %s: %s", want, req)
		}
	}
	edit, _ := json.Marshal(buildImageEditPayload("make it blue", []string{"meta-1"}, "1:1"))
	if !strings.Contains(string(edit), `"modelName":"imagine-image-edit"`) || !strings.Contains(string(edit), `"inputAssets":["meta-1"]`) {
		t.Fatalf("edit = %s", edit)
	}
	video, _ := json.Marshal(videoCreatePayload("run", "16:9", "720p", 6, nil, nil))
	if !strings.Contains(string(video), `"modelName":"imagine-video-gen"`) || !strings.Contains(string(video), `"textToVideo"`) {
		t.Fatalf("video = %s", video)
	}
	one := videoRequestFromInput(normalizedChatInput{Prompt: "shake", Attachments: []chatAttachmentInput{{Source: "https://x/a.png", Image: true}}}, "1:1")
	if one.ImageURL == "" || len(one.ReferenceURLs) != 0 {
		t.Fatalf("single image should be first-frame: %#v", one)
	}
	many := videoRequestFromInput(normalizedChatInput{Prompt: "same character", Attachments: []chatAttachmentInput{
		{Source: "https://x/a.png", Image: true},
		{Source: "https://x/b.png", Image: true},
	}}, "1:1")
	if many.ImageURL != "" || len(many.ReferenceURLs) != 2 {
		t.Fatalf("multi image should be references: %#v", many)
	}
	refPayload, _ := json.Marshal(videoCreatePayload("same", "1:1", "720p", 6, nil, []string{"meta-1", "meta-2"}))
	if !strings.Contains(string(refPayload), `"referenceToVideo"`) || strings.Contains(string(refPayload), `"imageToVideo"`) {
		t.Fatalf("ref video = %s", refPayload)
	}
}

func TestParseVideoStreamReadsStreamingResponse(t *testing.T) {
	body := `{"result":{"response":{"streamingVideoGenerationResponse":{"progress":40}}}}` +
		`{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoUrl":"users/test/generated/clip.mp4"}}}}`
	got, err := parseVideoStream(strings.NewReader(body))
	if err != nil || !strings.HasSuffix(got.URL, "/generated/clip.mp4") {
		t.Fatalf("got=%#v err=%v", got, err)
	}
}

func TestParseSessionIdentity(t *testing.T) {
	id, err := parseSession([]byte(`{"session":{"userId":"497f19f8-49d4-458a-bee4-43ec3dcaf8ca","email":"a@example.com"}}`))
	if err != nil || id.UserID != "497f19f8-49d4-458a-bee4-43ec3dcaf8ca" || id.Email != "a@example.com" {
		t.Fatalf("%#v %v", id, err)
	}
	if _, err := parseSession([]byte(`{"status":"unauthenticated"}`)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unauthenticated: %v", err)
	}
}
