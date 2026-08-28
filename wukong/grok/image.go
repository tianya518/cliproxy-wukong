package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type ImageResult struct {
	URLs []string
}

type imagineModelConfig struct {
	Pro            bool
	ExpectedCount  int
	MaxReturnCount int
}

type imagineImageValue struct {
	ID       string
	URL      string
	Blob     string
	Position int
	Width    int
	Height   int
	position bool
}

type imagineSlot struct {
	image        imagineImageValue
	final        bool
	completed    bool
	moderated    bool
	previewReady bool
}

type imagineCollector struct {
	slots         map[string]*imagineSlot
	terminalCount int
}

func resolveImagineModel(model string, pro bool, count int) (imagineModelConfig, bool) {
	if model != "imagine" {
		return imagineModelConfig{}, false
	}
	return imagineModelConfig{Pro: pro, ExpectedCount: count, MaxReturnCount: 10}, true
}

func (c *Client) GenerateImage(ctx context.Context, model, prompt, size string, count int) (ImageResult, error) {
	spec, ok := Resolve(model)
	if !ok || spec.Capability != CapabilityImage {
		return ImageResult{}, fmt.Errorf("模型不支持图片生成")
	}
	if count <= 0 {
		count = 1
	}
	if spec.ProtocolModel == "imagine-lite" {
		if count > maxGeneratedImages {
			return ImageResult{}, fmt.Errorf("n 不能超过 10")
		}
		return c.generateLiteImages(ctx, spec, prompt, count)
	}
	ratio, err := resolveImageAspectRatio("", size)
	if err != nil {
		return ImageResult{}, err
	}
	modelConfig, ok := resolveImagineModel(spec.ProtocolModel, spec.ImaginePro, count)
	if !ok {
		return ImageResult{}, fmt.Errorf("模型不支持图片生成")
	}
	if count > modelConfig.MaxReturnCount {
		return ImageResult{}, fmt.Errorf("n 不能超过 %d", modelConfig.MaxReturnCount)
	}
	return c.generateWSImage(ctx, prompt, ratio, count, modelConfig)
}

func (c *Client) generateLiteImages(ctx context.Context, spec ModelSpec, prompt string, count int) (ImageResult, error) {
	urls := make([]string, 0, count)
	for len(urls) < count {
		url, err := c.generateLiteImageURL(ctx, spec, prompt)
		if err != nil {
			if len(urls) == 0 {
				return ImageResult{}, err
			}
			return ImageResult{URLs: urls}, fmt.Errorf("Lite 图片仅完成 %d/%d 张: %w", len(urls), count, err)
		}
		urls = append(urls, url)
	}
	return ImageResult{URLs: urls}, nil
}

func (c *Client) generateLiteImageURL(ctx context.Context, spec ModelSpec, prompt string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		c.prepareMediaSession(ctx)
		url, err := c.generateLiteImageURLAttempt(ctx, spec, prompt)
		if err == nil {
			return url, nil
		}
		lastErr = err
		if !c.recoverMedia(ctx, err, attempt) {
			return "", err
		}
	}
	return "", lastErr
}

func (c *Client) generateLiteImageURLAttempt(ctx context.Context, spec ModelSpec, prompt string) (string, error) {
	source, err := c.openGatewayStream(ctx, spec, "Drawing: "+prompt, nil, nil)
	if err != nil {
		return "", err
	}
	defer source.Close()
	capture := &boundedCapture{limit: 8 << 20}
	var parsed parsedChat
	if err := consumeUpstreamInto(io.TeeReader(source, capture), &parsed, nil); err != nil {
		return "", err
	}
	if len(parsed.Images) == 0 {
		parsed.Images = extractMarkdownImages(parsed.Text.String())
	}
	if len(parsed.Images) == 0 {
		parsed.Images = extractCapturedImageURLs(capture.Bytes())
	}
	if len(parsed.Images) == 0 {
		return "", fmt.Errorf("Lite 图片未返回可用地址")
	}
	return parsed.Images[0], nil
}

func (c *Client) generateWSImage(ctx context.Context, prompt, ratio string, count int, modelConfig imagineModelConfig) (ImageResult, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		c.prepareMediaSession(ctx)
		result, err := c.generateWSImageAttempt(ctx, prompt, ratio, count, modelConfig)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !c.recoverMedia(ctx, err, attempt) {
			return ImageResult{}, err
		}
	}
	return ImageResult{}, lastErr
}

func (c *Client) generateWSImageAttempt(ctx context.Context, prompt, ratio string, count int, modelConfig imagineModelConfig) (ImageResult, error) {
	wsURL, err := imagineURL(c.baseURL())
	if err != nil {
		return ImageResult{}, err
	}
	headers := gatewayHeaders(c.baseURL(), "", c.cred.AccessToken(), c.cloudflareCookies(), c.userAgent())
	headers.Del("Cookie")
	headers.Set("Cookie", BuildSSOCookie(c.cred.AccessToken(), c.cloudflareCookies()))
	reqCtx, cancel := context.WithTimeout(ctx, c.cfg.ImageTimeout)
	defer cancel()
	connection, handshake, err := c.dialWS(reqCtx, wsURL, headers)
	if err != nil {
		if handshake != nil {
			return ImageResult{}, readMediaStatusError("连接 Imagine WebSocket", handshake)
		}
		return ImageResult{}, fmt.Errorf("连接 Imagine WebSocket: %w", err)
	}
	defer connection.Close()
	connection.SetReadLimit(64 << 20)
	deadline := time.Now().Add(c.cfg.ImageTimeout)
	_ = connection.SetReadDeadline(deadline)
	_ = connection.SetWriteDeadline(deadline)
	if err := connection.WriteJSON(imagineResetMessage()); err != nil {
		return ImageResult{}, err
	}
	if err := connection.WriteJSON(imagineRequestMessage(newWebID("img"), prompt, ratio, c.cfg.AllowNSFW, modelConfig.Pro, modelConfig.ExpectedCount)); err != nil {
		return ImageResult{}, err
	}
	collector := newImagineCollector()
	for collector.UsableCount() < count && !collector.Done(modelConfig.ExpectedCount) {
		messageType, data, readErr := connection.ReadMessage()
		if readErr != nil {
			return ImageResult{}, fmt.Errorf("读取 Imagine WebSocket: %w", readErr)
		}
		if messageType != websocket.TextMessage {
			continue
		}
		var message map[string]any
		if json.Unmarshal(data, &message) != nil {
			continue
		}
		if message["type"] == "error" {
			return ImageResult{}, fmt.Errorf("Imagine WebSocket 返回错误")
		}
		collector.Accept(message)
	}
	images := collector.Images()
	if len(images) == 0 {
		return ImageResult{}, fmt.Errorf("Imagine WebSocket 完成但没有可用图片")
	}
	urls := make([]string, 0, len(images))
	for _, image := range images {
		if image.URL != "" {
			urls = append(urls, image.URL)
		}
	}
	return ImageResult{URLs: urls}, nil
}

func (c *Client) EditImage(ctx context.Context, prompt string, imageURLs []string, size string) (ImageResult, error) {
	if len(imageURLs) == 0 || len(imageURLs) > 8 {
		return ImageResult{}, fmt.Errorf("image 数量必须在 1 到 8 之间")
	}
	ratio, err := resolveImageEditAspectRatio("", size)
	if err != nil {
		return ImageResult{}, err
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		c.prepareMediaSession(ctx)
		result, err := c.editImageAttempt(ctx, prompt, imageURLs, ratio)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !c.recoverMedia(ctx, err, attempt) {
			return ImageResult{}, err
		}
	}
	return ImageResult{}, lastErr
}

func (c *Client) editImageAttempt(ctx context.Context, prompt string, imageURLs []string, ratio string) (ImageResult, error) {
	assets := make([]string, 0, len(imageURLs))
	for _, rawURL := range imageURLs {
		image, loadErr := c.loadChatImage(ctx, rawURL)
		if loadErr != nil {
			return ImageResult{}, loadErr
		}
		uploaded, uploadErr := c.uploadFileV2Direct(ctx, image, c.baseURL()+"/imagine", imagineSelfUploadSource)
		if uploadErr != nil {
			return ImageResult{}, uploadErr
		}
		if uploaded.MetadataID == "" {
			return ImageResult{}, fmt.Errorf("上传图片成功但上游未返回 fileMetadataId")
		}
		assets = append(assets, uploaded.MetadataID)
	}
	payload, _ := json.Marshal(buildImageEditPayload(prompt, assets, ratio))
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := c.doJSON(ctx, c.baseURL()+"/rest/app-chat/conversations/new", payload, c.baseURL()+"/imagine", c.cfg.ImageTimeout, true)
		if err != nil {
			return ImageResult{}, err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			capture := &boundedCapture{limit: 8 << 20}
			var parsed parsedChat
			consumeErr := consumeUpstreamInto(io.TeeReader(resp.Body, capture), &parsed, nil)
			_ = resp.Body.Close()
			if consumeErr != nil {
				return ImageResult{}, consumeErr
			}
			urls := imageEditResultURLs(&parsed, capture.Bytes())
			if len(urls) == 0 {
				return ImageResult{}, fmt.Errorf("上游未返回可用的编辑图片")
			}
			return ImageResult{URLs: urls}, nil
		}
		mediaErr := readMediaStatusError("图片编辑", resp)
		lastErr = mediaErr
		if attempt == 0 && isPageOutOfDate(mediaErr.status, mediaErr.body) {
			c.statsig.invalidate()
			continue
		}
		return ImageResult{}, mediaErr
	}
	return ImageResult{}, lastErr
}

func newImagineCollector() *imagineCollector {
	return &imagineCollector{slots: make(map[string]*imagineSlot)}
}

func (col *imagineCollector) Accept(message map[string]any) {
	typeName, _ := message["type"].(string)
	if typeName != "image" && typeName != "json" {
		return
	}
	rawURL, _ := message["url"].(string)
	imageID := firstString(message, "image_id", "job_id", "id")
	if imageID == "" && rawURL != "" {
		imageID = imageIDFromURL(rawURL)
	}
	if imageID == "" {
		return
	}
	slot := col.slots[imageID]
	if slot == nil {
		slot = &imagineSlot{image: imagineImageValue{ID: imageID}}
		col.slots[imageID] = slot
	}
	if typeName == "image" {
		if position, ok := firstInt(message, "side_by_side_index", "order", "grid_index"); ok {
			slot.image.Position = position
			slot.image.position = true
		}
		progress, hasProgress := numberAsInt(message["percentage_complete"])
		if hasProgress && progress < 100 {
			slot.previewReady = true
			return
		}
		slot.image.URL = absoluteAssetURL(rawURL)
		slot.image.Blob, _ = message["blob"].(string)
		slot.final = true
		return
	}
	status, _ := message["current_status"].(string)
	if rawURL != "" && !slot.final {
		slot.image.URL = absoluteAssetURL(rawURL)
		slot.image.Blob, _ = message["blob"].(string)
		slot.final = true
	}
	if status == "completed" && !slot.completed {
		slot.completed = true
		col.terminalCount++
	}
	slot.moderated, _ = message["moderated"].(bool)
}

func (col *imagineCollector) Done(expected int) bool {
	return expected > 0 && col.terminalCount >= expected
}

func (col *imagineCollector) UsableCount() int {
	count := 0
	for _, slot := range col.slots {
		if slot.completed && !slot.moderated && slot.final && (slot.image.URL != "" || slot.image.Blob != "") {
			count++
		}
	}
	return count
}

func (col *imagineCollector) Images() []imagineImageValue {
	values := make([]imagineImageValue, 0, len(col.slots))
	for _, slot := range col.slots {
		if slot.completed && !slot.moderated && slot.final && (slot.image.URL != "" || slot.image.Blob != "") {
			values = append(values, slot.image)
		}
	}
	sort.SliceStable(values, func(i, j int) bool {
		if values[i].position != values[j].position {
			return values[i].position
		}
		if values[i].Position != values[j].Position {
			return values[i].Position < values[j].Position
		}
		return values[i].ID < values[j].ID
	})
	return values
}

func imagineURL(baseURL string) (string, error) {
	value, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	switch value.Scheme {
	case "https":
		value.Scheme = "wss"
	case "http":
		value.Scheme = "ws"
	default:
		return "", fmt.Errorf("Grok Web Base URL 协议无效")
	}
	value.Path = "/ws/imagine/listen"
	value.RawQuery = ""
	return value.String(), nil
}

func imagineResetMessage() map[string]any {
	return map[string]any{"type": "conversation.item.create", "timestamp": time.Now().UnixMilli(), "item": map[string]any{"type": "message", "content": []any{map[string]any{"type": "reset"}}}}
}

func imagineRequestMessage(id, prompt, ratio string, nsfw, pro bool, generations int) map[string]any {
	return map[string]any{"type": "conversation.item.create", "timestamp": time.Now().UnixMilli(), "item": map[string]any{"type": "message", "content": []any{map[string]any{"requestId": id, "text": prompt, "type": "input_text", "properties": map[string]any{"section_count": 0, "is_kids_mode": false, "enable_nsfw": nsfw, "skip_upsampler": false, "enable_side_by_side": true, "is_initial": false, "aspect_ratio": ratio, "enable_pro": pro, "num_generations": generations}}}}}
}

func buildImageEditPayload(prompt string, assets []string, aspectRatio string) map[string]any {
	imageToImage := map[string]any{"prompt": prompt, "inputAssets": assets}
	if aspectRatio != "" {
		imageToImage["aspectRatio"] = aspectRatio
	}
	return map[string]any{
		"modelName": "imagine-image-edit", "message": prompt,
		"enableImageStreaming": true, "enableSideBySide": true, "sendFinalMetadata": true,
		"mediaGenInput": map[string]any{"imageToImage": imageToImage},
	}
}

func imageEditResultURLs(parsed *parsedChat, captured []byte) []string {
	values := append([]string(nil), parsed.Images...)
	if len(values) == 0 {
		values = extractCapturedImageURLs(captured)
	}
	if len(values) == 0 {
		values = extractMarkdownImages(parsed.Text.String())
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = absoluteAssetURL(value)
		if value == "" {
			continue
		}
		if _, moderated := parsed.moderatedImages[value]; moderated || containsString(result, value) {
			continue
		}
		result = append(result, value)
	}
	return result
}

type boundedCapture struct {
	data  []byte
	limit int
}

func (w *boundedCapture) Write(value []byte) (int, error) {
	remaining := w.limit - len(w.data)
	if remaining > 0 {
		w.data = append(w.data, value[:min(remaining, len(value))]...)
	}
	return len(value), nil
}

func (w *boundedCapture) Bytes() []byte { return w.data }

func extractCapturedImageURLs(data []byte) []string {
	results := make([]string, 0, 2)
	_ = consumeJSONObjects(bytes.NewReader(data), 8<<20, func(frame []byte) error {
		var value any
		if json.Unmarshal(frame, &value) == nil {
			collectCapturedImageURLs(value, &results)
		}
		return nil
	})
	return results
}

func collectCapturedImageURLs(value any, results *[]string) {
	switch current := value.(type) {
	case map[string]any:
		if rawURL := imageURLFromCardData(current); rawURL != "" {
			appendCapturedImageURL(results, rawURL)
		}
		moderated, _ := current["moderated"].(bool)
		progress, hasProgress := numberAsInt(current["progress"])
		if !moderated && hasProgress && progress >= 100 {
			appendCapturedImageURL(results, firstString(current, "imageUrl", "image_url", "url"))
		}
		for _, nested := range current {
			collectCapturedImageURLs(nested, results)
		}
	case []any:
		for _, nested := range current {
			collectCapturedImageURLs(nested, results)
		}
	case string:
		trimmed := strings.TrimSpace(current)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			var nested any
			if json.Unmarshal([]byte(trimmed), &nested) == nil {
				collectCapturedImageURLs(nested, results)
				return
			}
		}
		appendCapturedImageURL(results, trimmed)
	}
}

func appendCapturedImageURL(results *[]string, value string) {
	value = strings.TrimSpace(value)
	if !strings.Contains(value, "/generated/") || strings.Contains(value, "-part-") || strings.ContainsAny(value, "{}[]\"") {
		return
	}
	if !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "users/") && !strings.HasPrefix(value, "/users/") {
		return
	}
	value = absoluteAssetURL(value)
	if !containsString(*results, value) {
		*results = append(*results, value)
	}
}

func resolveImageAspectRatio(aspectRatio, size string) (string, error) {
	values := map[string]string{
		"auto": "auto", "1:1": "1:1", "16:9": "16:9", "9:16": "9:16", "4:3": "4:3", "3:4": "3:4",
		"3:2": "3:2", "2:3": "2:3", "2:1": "2:1", "1:2": "1:2", "19.5:9": "19.5:9", "9:19.5": "9:19.5", "20:9": "20:9", "9:20": "9:20",
		"1280x720": "16:9", "720x1280": "9:16", "1792x1024": "3:2", "1536x1024": "3:2", "1024x1792": "2:3", "1024x1536": "2:3", "1024x1024": "1:1",
	}
	value := strings.ToLower(strings.TrimSpace(aspectRatio))
	if value == "" {
		value = strings.ToLower(strings.TrimSpace(size))
	}
	if value == "" {
		return "auto", nil
	}
	if resolved := values[value]; resolved != "" {
		return resolved, nil
	}
	return "", fmt.Errorf("aspect_ratio 不受支持")
}

func resolveImageEditAspectRatio(aspectRatio, size string) (string, error) {
	if strings.TrimSpace(aspectRatio) == "" && strings.TrimSpace(size) == "" {
		return "", nil
	}
	return resolveImageAspectRatio(aspectRatio, size)
}

func resolveAspectRatio(size string) string {
	if strings.TrimSpace(size) == "" {
		return "1:1"
	}
	value, err := resolveImageAspectRatio("", size)
	if err != nil {
		return "1:1"
	}
	return value
}

func imageIDFromURL(value string) string {
	parts := strings.Split(strings.Trim(value, "/"), "/")
	if len(parts) == 0 {
		return value
	}
	name := parts[len(parts)-1]
	if index := strings.IndexByte(name, '.'); index > 0 {
		return name[:index]
	}
	return name
}
