package grok

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	grokRenderPattern = regexp.MustCompile(`(?s)<grok:render\s+card_id="([^"]+)"\s+card_type="([^"]+)"\s+type="([^"]+)"[^>]*>.*?</grok:render>`)
)

func consumeJSONObjects(source io.Reader, maxObjectBytes int, consume func([]byte) error) error {
	reader := bufio.NewReaderSize(source, 64<<10)
	frame := make([]byte, 0, 64<<10)
	depth := 0
	inString := false
	escaped := false
	for {
		value, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if depth != 0 {
					return io.ErrUnexpectedEOF
				}
				return nil
			}
			return err
		}
		if depth == 0 {
			if value != '{' {
				continue
			}
			frame = frame[:0]
			depth = 1
			inString = false
			escaped = false
			frame = append(frame, value)
			continue
		}
		frame = append(frame, value)
		if len(frame) > maxObjectBytes {
			return fmt.Errorf("Grok Web 单个响应帧超过 %d MiB", maxObjectBytes>>20)
		}
		if inString {
			if escaped {
				escaped = false
			} else if value == '\\' {
				escaped = true
			} else if value == '"' {
				inString = false
			}
			continue
		}
		switch value {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				if err := consume(frame); err != nil {
					return err
				}
			}
		}
	}
}

func consumeUpstreamInto(source io.Reader, parsed *parsedChat, emit func(string, string) error) error {
	return consumeJSONObjects(source, 8<<20, func(data []byte) error {
		kind, delta, err := parseUpstreamFrame(data, parsed)
		if err != nil {
			return err
		}
		if emit == nil {
			return nil
		}
		return emit(kind, delta)
	})
}

func parseUpstreamFrame(data []byte, parsed *parsedChat) (string, string, error) {
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return "", "", nil
	}
	if event, ok := root["event"].(map[string]any); ok {
		return parseGatewayEvent(event, parsed)
	}
	if errorValue, ok := root["error"].(map[string]any); ok {
		return "", "", webResponseError(errorValue)
	}
	result, _ := root["result"].(map[string]any)
	if conversation, _ := result["conversation"].(map[string]any); conversation != nil {
		parsed.ConversationID, _ = conversation["conversationId"].(string)
		return "", "", nil
	}
	response, _ := result["response"].(map[string]any)
	if response == nil {
		return "", "", nil
	}
	if errorValue, ok := response["error"].(map[string]any); ok {
		return "", "", webResponseError(errorValue)
	}
	for _, key := range []string{"cardAttachment", "cardAttachments"} {
		if rawURL := collectCardAttachment(parsed, response[key]); rawURL != "" {
			rawURL = absoluteAssetURL(rawURL)
			parsed.Images = appendUniqueString(parsed.Images, rawURL)
			return "image", rawURL, nil
		}
	}
	if userResponse, _ := response["userResponse"].(map[string]any); userResponse != nil {
		if id, _ := userResponse["responseId"].(string); id != "" {
			parsed.ParentID = id
		}
	}
	collectSearchSources(parsed, response)
	token, _ := response["token"].(string)
	thinking, _ := response["isThinking"].(bool)
	tag, _ := response["messageTag"].(string)
	if tag == "tool_usage_card" {
		return "", "", nil
	}
	if token != "" && thinking {
		parsed.Reasoning.WriteString(token)
		return "reasoning", token, nil
	}
	if token != "" && !thinking && (tag == "final" || tag == "") {
		parsed.upstreamText.WriteString(token)
		cleaned := cleanChatToken(parsed, token)
		parsed.appendText(cleaned)
		return "text", cleaned, nil
	}
	if modelResponse, _ := response["modelResponse"].(map[string]any); modelResponse != nil {
		return collectModelResponse(parsed, modelResponse)
	}
	if imageResponse, _ := response["streamingImageGenerationResponse"].(map[string]any); imageResponse != nil {
		rawURL, _ := imageResponse["imageUrl"].(string)
		if rawURL == "" {
			rawURL, _ = imageResponse["url"].(string)
		}
		if rawURL != "" {
			moderated, _ := imageResponse["moderated"].(bool)
			if moderated {
				markModeratedImage(parsed, rawURL)
				return "", "", nil
			}
			completed, _ := imageResponse["isFinal"].(bool)
			progress, hasProgress := numberAsInt(imageResponse["progress"])
			if completed || (hasProgress && progress >= 100) {
				rawURL = absoluteAssetURL(rawURL)
				parsed.Images = appendUniqueString(parsed.Images, rawURL)
				return "image", rawURL, nil
			}
		}
	}
	return "", "", nil
}

func parseGatewayEvent(event map[string]any, parsed *parsedChat) (string, string, error) {
	typeName, _ := event["type"].(string)
	switch typeName {
	case "conversation.attached":
		conversation, _ := event["conversation"].(map[string]any)
		parsed.ConversationID, _ = conversation["id"].(string)
	case "response.chunk":
		chunk, _ := event["chunk"].(map[string]any)
		if chunk == nil {
			return "", "", nil
		}
		if card, _ := chunk["tool_usage_card"].(map[string]any); card != nil {
			collectGatewayToolUsageCard(parsed, card)
		}
		if result, _ := chunk["tool_result"].(map[string]any); result != nil {
			collectGatewayToolResult(parsed, result)
		}
		if cite, _ := chunk["render_citation"].(map[string]any); cite != nil {
			return applyGatewayRenderCitation(parsed, cite)
		}
		text, _ := chunk["text"].(map[string]any)
		delta, _ := text["text"].(string)
		channel, _ := text["channel"].(string)
		return appendGatewayDelta(parsed, channel, delta)
	case "response.output_text.delta":
		delta, _ := event["delta"].(string)
		return appendGatewayDelta(parsed, "CHANNEL_ASSISTANT_RESPONSE", delta)
	case "response.output_text.done":
		text, _ := event["text"].(string)
		if parsed.upstreamText.Len() == 0 && text != "" {
			return appendGatewayDelta(parsed, "CHANNEL_ASSISTANT_RESPONSE", text)
		}
	case "response.done":
		response, _ := event["response"].(map[string]any)
		parsed.ParentID, _ = response["id"].(string)
		status, _ := response["status"].(string)
		if status != "" && status != "completed" && !parsed.hasVisibleOutput() {
			return "", "", &GatewayStatusError{Status: status}
		}
	case "response.search.result":
		result, _ := event["result"].(map[string]any)
		if rawURL, _ := result["url"].(string); rawURL != "" {
			appendSearchSource(parsed, rawURL, firstString(result, "title"), "web")
		}
	case "response.grok.output":
		output, _ := event["output"].(map[string]any)
		if streamError, _ := output["stream_error"].(map[string]any); streamError != nil {
			return "", "", webResponseError(streamError)
		}
		if rawURL := collectCardAttachment(parsed, output["card_attachment"]); rawURL != "" {
			rawURL = absoluteAssetURL(rawURL)
			parsed.Images = appendUniqueString(parsed.Images, rawURL)
			return "image", rawURL, nil
		}
	case "error":
		return "", "", gatewayEventError(event)
	}
	return "", "", nil
}

func appendGatewayDelta(parsed *parsedChat, channel, delta string) (string, string, error) {
	if delta == "" {
		return "", "", nil
	}
	normalized := strings.ToUpper(strings.TrimSpace(channel))
	if strings.Contains(normalized, "ANALYSIS") || strings.Contains(normalized, "REASONING") {
		parsed.Reasoning.WriteString(delta)
		return "reasoning", delta, nil
	}
	if normalized != "" && normalized != "CHANNEL_ASSISTANT_RESPONSE" {
		return "", "", nil
	}
	parsed.upstreamText.WriteString(delta)
	cleaned := cleanChatToken(parsed, delta)
	parsed.appendText(cleaned)
	return "text", cleaned, nil
}

func collectGatewayToolUsageCard(parsed *parsedChat, card map[string]any) {
	if parsed == nil || card == nil {
		return
	}
	id := firstString(card, "tool_usage_card_id", "id")
	if id == "" {
		id = fmt.Sprintf("card:%d", parsed.ServerTools+1)
	}
	if web, _ := card["web_search"].(map[string]any); web != nil {
		recordGatewaySearchTool(parsed, id, "web_search")
		upsertHostedSearchCall(parsed, id, "web_search", nestedSearchQuery(web), "in_progress")
		return
	}
	if xSearch, _ := card["x_search"].(map[string]any); xSearch != nil {
		recordGatewaySearchTool(parsed, id, "x_search")
		upsertHostedSearchCall(parsed, id, "x_search", nestedSearchQuery(xSearch), "in_progress")
		return
	}
	recordGatewaySearchTool(parsed, id, "")
}

func recordGatewaySearchTool(parsed *parsedChat, id, kind string) {
	if parsed == nil || id == "" {
		return
	}
	if parsed.serverToolKeys == nil {
		parsed.serverToolKeys = make(map[string]struct{})
	}
	if _, exists := parsed.serverToolKeys[id]; !exists {
		if len(parsed.serverToolKeys) >= maxTrackedServerTools {
			return
		}
		parsed.serverToolKeys[id] = struct{}{}
		parsed.ServerTools++
	}
	var keys *map[string]struct{}
	var count *int64
	switch kind {
	case "web_search":
		keys, count = &parsed.webSearchKeys, &parsed.WebSearchTools
	case "x_search":
		keys, count = &parsed.xSearchKeys, &parsed.XSearchTools
	default:
		return
	}
	if *keys == nil {
		*keys = make(map[string]struct{})
	}
	if _, exists := (*keys)[id]; exists || len(*keys) >= maxTrackedServerTools {
		return
	}
	(*keys)[id] = struct{}{}
	*count++
}

func nestedSearchQuery(tool map[string]any) string {
	if tool == nil {
		return ""
	}
	if q := firstString(tool, "query"); q != "" {
		return q
	}
	if args, _ := tool["args"].(map[string]any); args != nil {
		return firstString(args, "query")
	}
	return ""
}

func collectGatewayToolResult(parsed *parsedChat, result map[string]any) {
	if parsed == nil || result == nil {
		return
	}
	callID := firstString(result, "tool_call_id", "tool_usage_card_id", "id")
	if web, _ := result["web_search"].(map[string]any); web != nil {
		var sources []map[string]any
		pages, _ := web["webpages"].([]any)
		for _, raw := range pages {
			item, _ := raw.(map[string]any)
			if item == nil {
				continue
			}
			u := firstString(item, "url")
			title := firstString(item, "title")
			appendSearchSource(parsed, u, title, "web")
			if normalized, ok := normalizeURL(u); ok {
				sources = append(sources, map[string]any{
					"type": "url", "url": normalized, "title": normalizeTitle(title, normalized),
				})
			}
		}
		call := upsertHostedSearchCall(parsed, callID, "web_search", nestedSearchQuery(web), "completed")
		appendHostedSearchSources(call, sources)
		if call != nil {
			call.Status = "completed"
			recordGatewaySearchTool(parsed, call.ID, "web_search")
		}
	}
	if xPost, _ := result["x_post"].(map[string]any); xPost != nil {
		var sources []map[string]any
		posts, _ := xPost["posts"].([]any)
		for _, raw := range posts {
			item, _ := raw.(map[string]any)
			if item == nil {
				continue
			}
			handle := firstString(item, "userhandle", "username")
			postID := firstString(item, "post_id", "postId")
			if handle == "" || postID == "" {
				continue
			}
			rawURL := "https://x.com/" + url.PathEscape(handle) + "/status/" + url.PathEscape(postID)
			title := firstString(item, "name", "userhandle", "username")
			if text, _ := item["text"].(string); text != "" && title != "" {
				title = title + ": " + text
			} else if text, _ := item["text"].(string); text != "" {
				title = text
			}
			appendSearchSource(parsed, rawURL, title, "x_post")
			if normalized, ok := normalizeURL(rawURL); ok {
				sources = append(sources, map[string]any{
					"type": "url", "url": normalized, "title": normalizeTitle(title, normalized),
				})
			}
		}
		call := upsertHostedSearchCall(parsed, callID, "x_search", "", "completed")
		appendHostedSearchSources(call, sources)
		if call != nil {
			call.Status = "completed"
			if call.Kind == "" {
				call.Kind = "x_search"
			}
			recordGatewaySearchTool(parsed, call.ID, "x_search")
		}
	}
}

func applyGatewayRenderCitation(parsed *parsedChat, cite map[string]any) (string, string, error) {
	if parsed == nil || cite == nil {
		return "", "", nil
	}
	rawURL := firstString(cite, "url")
	normalized, valid := normalizeURL(rawURL)
	if !valid {
		return "", "", nil
	}
	if parsed.citationIndex == nil {
		parsed.citationIndex = make(map[string]int)
	}
	index, exists := parsed.citationIndex[normalized]
	if !exists {
		if len(parsed.citationIndex) >= maxTrackedCitationSources {
			return "", "", nil
		}
		index = len(parsed.citationIndex) + 1
		parsed.citationIndex[normalized] = index
	}
	if parsed.lastCitation == index {
		return "", "", nil
	}
	parsed.lastCitation = index
	annotation := citationAnnotation(parsed, normalized, index)
	if parsed.DisableInlineCitations {
		if len(parsed.Annotations) < maxTrackedAnnotations {
			parsed.Annotations = append(parsed.Annotations, annotation)
		}
		return "", "", nil
	}
	replacement := fmt.Sprintf("[[%d]](%s)", index, normalized)
	start := parsed.textCharacterLen()
	parsed.upstreamText.WriteString(replacement)
	parsed.appendText(replacement)
	annotation["start_index"] = start
	annotation["end_index"] = start + utf8.RuneCountInString(replacement)
	if len(parsed.Annotations) < maxTrackedAnnotations {
		parsed.Annotations = append(parsed.Annotations, annotation)
	}
	return "text", replacement, nil
}

func citationAnnotation(parsed *parsedChat, rawURL string, index int) map[string]any {
	if index < 1 {
		index = 1
	}
	annotation := map[string]any{
		"type": "url_citation", "url": rawURL, "title": fmt.Sprintf("%d", index),
	}
	if parsed != nil {
		if title := lookupSourcePageTitle(parsed.SearchSources, rawURL); title != "" {
			annotation["source_title"] = title
			return annotation
		}
		for _, call := range parsed.HostedSearchCalls {
			if title := lookupSourcePageTitle(call.Sources, rawURL); title != "" {
				annotation["source_title"] = title
				return annotation
			}
		}
	}
	return annotation
}

func lookupSourcePageTitle(sources []map[string]any, rawURL string) string {
	if normalized, valid := normalizeURL(rawURL); valid {
		rawURL = normalized
	}
	for _, source := range sources {
		if value, _ := source["url"].(string); value == rawURL {
			if title, _ := source["title"].(string); title != "" {
				return title
			}
			return ""
		}
	}
	return ""
}

func upsertHostedSearchCall(parsed *parsedChat, id, kind, query, status string) *hostedSearchCall {
	if parsed == nil {
		return nil
	}
	if id == "" {
		matchedID := ""
		for i := range parsed.HostedSearchCalls {
			call := &parsed.HostedSearchCalls[i]
			if call.Kind != kind || call.Status == "completed" {
				continue
			}
			if matchedID != "" {
				matchedID = ""
				break
			}
			matchedID = call.ID
		}
		if matchedID != "" {
			id = matchedID
		} else {
			id = fmt.Sprintf("%s_%d", kind, len(parsed.HostedSearchCalls)+1)
		}
	}
	if parsed.hostedSearchByID == nil {
		parsed.hostedSearchByID = make(map[string]int)
	}
	if index, ok := parsed.hostedSearchByID[id]; ok {
		call := &parsed.HostedSearchCalls[index]
		if query != "" && call.Query == "" {
			call.Query = query
		}
		if status != "" {
			call.Status = status
		}
		if kind != "" && call.Kind == "" {
			call.Kind = kind
		}
		return call
	}
	parsed.HostedSearchCalls = append(parsed.HostedSearchCalls, hostedSearchCall{
		ID: id, Kind: kind, Query: query, Status: status,
	})
	parsed.hostedSearchByID[id] = len(parsed.HostedSearchCalls) - 1
	return &parsed.HostedSearchCalls[len(parsed.HostedSearchCalls)-1]
}

func appendHostedSearchSources(call *hostedSearchCall, sources []map[string]any) {
	if call == nil || len(sources) == 0 {
		return
	}
	call.Sources = append(call.Sources, sources...)
}

func appendSearchSource(parsed *parsedChat, value, title, sourceType string) {
	value, valid := normalizeURL(value)
	if !valid {
		return
	}
	if parsed.sourceKeys == nil {
		parsed.sourceKeys = make(map[string]struct{})
	}
	if _, exists := parsed.sourceKeys[value]; exists {
		return
	}
	if len(parsed.SearchSources) >= maxResults {
		return
	}
	parsed.sourceKeys[value] = struct{}{}
	parsed.SearchSources = append(parsed.SearchSources, map[string]any{
		"url": value, "title": normalizeTitle(title, value), "type": sourceType,
	})
}

func collectSearchSources(parsed *parsedChat, response map[string]any) {
	if parsed.sourceKeys == nil {
		parsed.sourceKeys = make(map[string]struct{})
	}
	collectWebSearchResults(parsed, response["webSearchResults"])
	collectWebSearchResults(parsed, response["citedWebSearchResults"])
	collectXSearchResults(parsed, response["xSearchResults"])
	collectXSearchResults(parsed, response["xposts"])
	collectXSearchResults(parsed, response["citedXposts"])
}

func collectWebSearchResults(parsed *parsedChat, value any) {
	if wrapped, _ := value.(map[string]any); wrapped != nil {
		value = wrapped["results"]
	}
	values, _ := value.([]any)
	for _, raw := range values {
		item, _ := raw.(map[string]any)
		rawURL, _ := item["url"].(string)
		if rawURL == "" {
			continue
		}
		title, _ := item["title"].(string)
		appendSearchSource(parsed, rawURL, title, "web")
	}
}

func collectXSearchResults(parsed *parsedChat, value any) {
	if wrapped, _ := value.(map[string]any); wrapped != nil {
		value = wrapped["results"]
	}
	values, _ := value.([]any)
	for _, raw := range values {
		item, _ := raw.(map[string]any)
		username, _ := item["username"].(string)
		postID, _ := item["postId"].(string)
		if username == "" || postID == "" {
			continue
		}
		title, _ := item["text"].(string)
		rawURL := "https://x.com/" + url.PathEscape(username) + "/status/" + url.PathEscape(postID)
		appendSearchSource(parsed, rawURL, title, "x_post")
	}
}

func collectCardAttachment(parsed *parsedChat, value any) string {
	if values, ok := value.([]any); ok {
		first := ""
		for _, item := range values {
			if rawURL := collectCardAttachment(parsed, item); first == "" && rawURL != "" {
				first = rawURL
			}
		}
		return first
	}
	data := cardAttachmentData(value)
	if data == nil {
		return ""
	}
	if id, _ := data["id"].(string); id != "" {
		if parsed.cardCache == nil {
			parsed.cardCache = make(map[string]map[string]any)
		}
		parsed.cardCache[id] = data
	}
	return imageURLFromCardData(data)
}

func cardAttachmentData(value any) map[string]any {
	card, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if raw, ok := card["jsonData"].(map[string]any); ok {
		return raw
	}
	if raw, _ := card["jsonData"].(string); raw != "" {
		var data map[string]any
		if json.Unmarshal([]byte(raw), &data) == nil {
			return data
		}
	}
	if card["image_chunk"] != nil || card["imageChunk"] != nil {
		return card
	}
	return nil
}

func imageURLFromCardData(data map[string]any) string {
	chunk, _ := data["image_chunk"].(map[string]any)
	if chunk == nil {
		chunk, _ = data["imageChunk"].(map[string]any)
	}
	if chunk == nil {
		return ""
	}
	moderated, _ := chunk["moderated"].(bool)
	progress, _ := numberAsInt(chunk["progress"])
	if moderated || progress < 100 {
		return ""
	}
	imageURL, _ := chunk["imageUrl"].(string)
	if imageURL == "" {
		imageURL, _ = chunk["image_url"].(string)
	}
	return imageURL
}

func collectModelResponse(parsed *parsedChat, modelResponse map[string]any) (string, string, error) {
	if err := modelResponseStreamError(modelResponse); err != nil {
		return "", "", err
	}
	if parsed.ParentID == "" {
		parsed.ParentID, _ = modelResponse["parentResponseId"].(string)
	}
	collectSearchSources(parsed, modelResponse)
	firstImage := collectModelResponseImages(parsed, modelResponse)
	message, _ := modelResponse["message"].(string)
	if delta := mergeModelResponseText(parsed, message); delta != "" {
		return "text", delta, nil
	}
	if firstImage != "" {
		return "image", firstImage, nil
	}
	return "", "", nil
}

func mergeModelResponseText(parsed *parsedChat, message string) string {
	if message == "" {
		return ""
	}
	raw := parsed.upstreamText.String()
	if raw == message || strings.HasPrefix(raw, message) {
		return ""
	}
	if raw != "" && !strings.HasPrefix(message, raw) {
		return ""
	}
	delta := message[len(raw):]
	parsed.upstreamText.WriteString(delta)
	delta = cleanChatToken(parsed, delta)
	parsed.appendText(delta)
	return delta
}

func modelResponseStreamError(modelResponse map[string]any) error {
	values, _ := modelResponse["streamErrors"].([]any)
	for _, raw := range values {
		switch value := raw.(type) {
		case string:
			if message := strings.TrimSpace(value); message != "" {
				return errors.New(message)
			}
		case map[string]any:
			if nested, _ := value["error"].(map[string]any); nested != nil {
				return webResponseError(nested)
			}
			if message := firstString(value, "message", "error", "detail"); message != "" {
				return webResponseError(map[string]any{"message": message, "code": value["code"]})
			}
		}
	}
	return nil
}

func collectModelResponseImages(parsed *parsedChat, modelResponse map[string]any) string {
	first := ""
	appendImage := func(value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		value = absoluteAssetURL(value)
		if _, moderated := parsed.moderatedImages[value]; moderated {
			return
		}
		if containsString(parsed.Images, value) {
			return
		}
		parsed.Images = append(parsed.Images, value)
		if first == "" {
			first = value
		}
	}
	if urls, ok := modelResponse["generatedImageUrls"].([]any); ok {
		for _, raw := range urls {
			value, _ := raw.(string)
			appendImage(value)
		}
	}
	if cards, ok := modelResponse["cardAttachmentsJson"].([]any); ok {
		for _, raw := range cards {
			encoded, _ := raw.(string)
			var card map[string]any
			if encoded == "" || json.Unmarshal([]byte(encoded), &card) != nil {
				continue
			}
			appendImage(imageURLFromCardData(card))
		}
	}
	return first
}

func markModeratedImage(parsed *parsedChat, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	if parsed.moderatedImages == nil {
		parsed.moderatedImages = make(map[string]struct{})
	}
	parsed.moderatedImages[absoluteAssetURL(value)] = struct{}{}
}

func cleanChatToken(parsed *parsedChat, token string) string {
	if !strings.Contains(token, "<grok:render") {
		if token != "" {
			parsed.lastCitation = 0
		}
		return token
	}
	matches := grokRenderPattern.FindAllStringSubmatchIndex(token, -1)
	if len(matches) == 0 {
		return token
	}
	var builder strings.Builder
	cursor := 0
	for _, match := range matches {
		prefix := token[cursor:match[0]]
		builder.WriteString(prefix)
		if prefix != "" {
			parsed.lastCitation = 0
		}
		cardID := token[match[2]:match[3]]
		renderType := token[match[6]:match[7]]
		replacement, _ := renderChatCard(parsed, cardID, renderType)
		builder.WriteString(replacement)
		cursor = match[1]
	}
	builder.WriteString(token[cursor:])
	return builder.String()
}

func renderChatCard(parsed *parsedChat, cardID, renderType string) (string, map[string]any) {
	if parsed.cardCache == nil {
		return "", nil
	}
	card := parsed.cardCache[cardID]
	if card == nil {
		return "", nil
	}
	switch renderType {
	case "render_generated_image", "render_file":
		return "", nil
	case "render_searched_image":
		image, _ := card["image"].(map[string]any)
		if image == nil {
			return "", nil
		}
		title, _ := image["title"].(string)
		thumbnail := firstString(image, "thumbnail", "original")
		link, _ := image["link"].(string)
		if thumbnail == "" {
			return "", nil
		}
		if title == "" {
			title = "image"
		}
		if link != "" {
			return fmt.Sprintf("[![%s](%s)](%s)", title, thumbnail, link), nil
		}
		return fmt.Sprintf("![%s](%s)", title, thumbnail), nil
	default:
		return "", nil
	}
}

func webResponseError(value map[string]any) error {
	message, _ := value["message"].(string)
	if message == "" {
		message = "Grok Web stream error"
	}
	code, _ := numberAsInt(value["code"])
	if code == 7 || strings.Contains(strings.ToLower(message), "anti-bot") {
		return fmt.Errorf("%w: %s", ErrAntiBot, message)
	}
	normalized := strings.ToLower(message)
	if strings.Contains(normalized, "usage limit") || strings.Contains(normalized, "usage quota") {
		return fmt.Errorf("%w: %s", ErrUsageLimit, message)
	}
	return errors.New(message)
}

func gatewayEventError(event map[string]any) error {
	errorValue, _ := event["error"].(map[string]any)
	if errorValue == nil {
		return errors.New("Grok Gateway 返回未知错误")
	}
	return webResponseError(errorValue)
}

func firstString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if result, _ := value[key].(string); result != "" {
			return result
		}
	}
	return ""
}

func firstInt(value map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		if result, ok := numberAsInt(value[key]); ok {
			return result, true
		}
	}
	return 0, false
}

func numberAsInt(value any) (int, bool) {
	switch number := value.(type) {
	case float64:
		return int(number), true
	case int:
		return number, true
	case json.Number:
		parsed, err := number.Int64()
		return int(parsed), err == nil
	default:
		return 0, false
	}
}

func appendUniqueString(values []string, value string) []string {
	if containsString(values, value) {
		return values
	}
	return append(values, value)
}

func containsString(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

func absoluteAssetURL(value string) string {
	if strings.HasPrefix(value, "https://") {
		return value
	}
	return "https://assets.grok.com/" + strings.TrimPrefix(value, "/")
}

func trustedImageAssetHost(host string) bool {
	return strings.EqualFold(host, "assets.grok.com") || strings.EqualFold(host, "imagine-public.x.ai") || strings.EqualFold(host, "imgen.x.ai")
}

func extractMarkdownImages(value string) []string {
	results := make([]string, 0, 2)
	for {
		start := strings.Index(value, "![image](")
		if start < 0 {
			break
		}
		value = value[start+len("![image]("):]
		end := strings.IndexByte(value, ')')
		if end < 0 {
			break
		}
		results = append(results, value[:end])
		value = value[end+1:]
	}
	return results
}
