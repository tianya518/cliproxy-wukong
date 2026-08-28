package grok

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type VideoResult struct {
	URL          string
	Conversation string
}

type VideoRequest struct {
	Prompt        string
	Size          string
	Resolution    string
	Duration      int
	ImageURL      string
	ReferenceURLs []string
}

func (c *Client) GenerateVideo(ctx context.Context, request VideoRequest) (VideoResult, error) {
	if strings.TrimSpace(request.ImageURL) != "" && len(request.ReferenceURLs) > 0 {
		return VideoResult{}, fmt.Errorf("image 不能与 reference_images 同时使用")
	}
	if request.Duration <= 0 {
		request.Duration = 6
	}
	if request.Duration < 1 || request.Duration > 15 {
		return VideoResult{}, fmt.Errorf("duration 必须在 1 到 15 秒之间")
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		c.prepareMediaSession(ctx)
		result, err := c.generateVideoAttempt(ctx, request)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !c.recoverMedia(ctx, err, attempt) {
			return VideoResult{}, err
		}
	}
	return VideoResult{}, lastErr
}

func (c *Client) generateVideoAttempt(ctx context.Context, request VideoRequest) (VideoResult, error) {
	resolution := request.Resolution
	if resolution == "" {
		resolution = "720p"
	}
	ratio := resolveAspectRatio(request.Size)
	imageAssets, err := c.uploadVideoInputAssets(ctx, videoFirstFrameURLs(request))
	if err != nil {
		return VideoResult{}, err
	}
	referenceAssets, err := c.uploadVideoInputAssets(ctx, request.ReferenceURLs)
	if err != nil {
		return VideoResult{}, err
	}
	payload, _ := json.Marshal(videoCreatePayload(request.Prompt, ratio, resolution, request.Duration, imageAssets, referenceAssets))
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := c.doJSON(ctx, c.baseURL()+"/rest/app-chat/conversations/new", payload, c.baseURL()+"/imagine", c.cfg.VideoTimeout, true)
		if err != nil {
			return VideoResult{}, err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			defer resp.Body.Close()
			return parseVideoStream(resp.Body)
		}
		mediaErr := readMediaStatusError("视频生成", resp)
		lastErr = mediaErr
		if attempt == 0 && isPageOutOfDate(mediaErr.status, mediaErr.body) {
			c.statsig.invalidate()
			continue
		}
		return VideoResult{}, mediaErr
	}
	return VideoResult{}, lastErr
}

func (c *Client) DownloadAsset(ctx context.Context, rawURL string) (io.ReadCloser, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme != "https" || !trustedImageAssetHost(parsed.Hostname()) || parsed.User != nil {
		return nil, "", fmt.Errorf("内容 URL 不受信任")
	}
	headers := c.restHeaders("", c.baseURL()+"/")
	headers.Del("Content-Type")
	resp, err := c.do(ctx, http.MethodGet, parsed.String(), nil, headers)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, "", fmt.Errorf("下载资源返回 %d", resp.StatusCode)
	}
	return resp.Body, resp.Header.Get("Content-Type"), nil
}

func (c *Client) uploadVideoInputAssets(ctx context.Context, urls []string) ([]string, error) {
	if len(urls) == 0 {
		return nil, nil
	}
	assets := make([]string, 0, len(urls))
	for _, rawURL := range urls {
		rawURL = strings.TrimSpace(rawURL)
		if rawURL == "" {
			continue
		}
		image, err := c.loadChatImage(ctx, rawURL)
		if err != nil {
			return nil, err
		}
		uploaded, err := c.uploadFileV2Direct(ctx, image, c.baseURL()+"/imagine", imagineSelfUploadSource)
		if err != nil {
			return nil, err
		}
		if uploaded.MetadataID == "" {
			return nil, fmt.Errorf("上传图片成功但上游未返回 fileMetadataId")
		}
		assets = append(assets, uploaded.MetadataID)
	}
	return assets, nil
}

const maxVideoReferenceImages = 7

func videoRequestFromInput(input normalizedChatInput, size string) VideoRequest {
	request := VideoRequest{Prompt: input.Prompt, Size: size}
	urls := make([]string, 0, len(input.Attachments))
	for _, att := range input.Attachments {
		if att.Image && strings.TrimSpace(att.Source) != "" {
			urls = append(urls, att.Source)
		}
	}
	switch {
	case len(urls) == 0:
	case len(urls) == 1:
		request.ImageURL = urls[0]
	default:
		if len(urls) > maxVideoReferenceImages {
			urls = urls[:maxVideoReferenceImages]
		}
		request.ReferenceURLs = urls
	}
	return request
}

func videoFirstFrameURLs(request VideoRequest) []string {
	if value := strings.TrimSpace(request.ImageURL); value != "" {
		return []string{value}
	}
	return nil
}

func videoCreatePayload(prompt, ratio, resolution string, seconds int, imageAssets, referenceAssets []string) map[string]any {
	return map[string]any{
		"modelName":            "imagine-video-gen",
		"message":              strings.TrimSpace(prompt + " --mode=custom"),
		"enableImageStreaming": true,
		"enableSideBySide":     true,
		"sendFinalMetadata":    true,
		"responseMetadata": map[string]any{
			"experiments":         []any{},
			"modelConfigOverride": map[string]any{"modelMap": map[string]any{}},
		},
		"mediaGenInput": videoMediaGenInput(prompt, ratio, resolution, seconds, imageAssets, referenceAssets),
		"kind":          "CONVERSATION_KIND_IMAGINE",
	}
}

func videoMediaGenInput(prompt, ratio, resolution string, seconds int, imageAssets, referenceAssets []string) map[string]any {
	if len(referenceAssets) > 0 {
		return map[string]any{"referenceToVideo": videoMediaParams(prompt, ratio, resolution, seconds, referenceAssets, false)}
	}
	if len(imageAssets) > 0 {
		return map[string]any{"imageToVideo": videoMediaParams(prompt, ratio, resolution, seconds, imageAssets, true)}
	}
	return map[string]any{"textToVideo": videoMediaParams(prompt, ratio, resolution, seconds, nil, false)}
}

func videoMediaParams(prompt, ratio, resolution string, seconds int, assets []string, firstFrame bool) map[string]any {
	params := map[string]any{
		"aspectRatio":    ratio,
		"duration":       seconds,
		"resolutionName": resolution,
	}
	if firstFrame {
		params["mode"] = "custom"
		if value := strings.TrimSpace(prompt); value != "" {
			params["prompt"] = value
		}
	} else {
		params["prompt"] = prompt
	}
	if len(assets) > 0 {
		params["inputAssets"] = assets
	}
	return params
}

func parseVideoStream(source io.Reader) (VideoResult, error) {
	var result VideoResult
	handle := func(root map[string]any) (bool, error) {
		if event, _ := root["event"].(map[string]any); event != nil && event["type"] == "error" {
			return false, gatewayEventError(event)
		}
		if errorValue, ok := root["error"].(map[string]any); ok {
			return false, webResponseError(errorValue)
		}
		if errorValue := nestedMap(root, "result", "response", "error"); errorValue != nil {
			return false, webResponseError(errorValue)
		}
		if conv, _ := nestedMap(root, "result", "conversation")["conversationId"].(string); conv != "" {
			result.Conversation = conv
		}
		stream := nestedMap(root, "result", "response", "streamingVideoGenerationResponse")
		if stream != nil {
			if moderated, _ := stream["moderated"].(bool); !moderated {
				if setVideoResultURL(&result, firstString(stream, "videoUrl", "contentUrl", "contentURL", "assetUrl", "assetURL", "fileUri", "fileURL")) {
					return true, nil
				}
			}
		}
		for _, attachment := range videoFileAttachments(root) {
			if setVideoResultURL(&result, attachment) {
				return true, nil
			}
		}
		response := nestedMap(root, "result", "response")
		if response != nil {
			if setVideoResultURL(&result, firstString(response, "videoUrl", "video_url")) {
				return true, nil
			}
			if model, _ := response["modelResponse"].(map[string]any); model != nil {
				if setVideoResultURL(&result, firstString(model, "videoUrl", "generatedVideoUrl")) {
					return true, nil
				}
			}
		}
		return false, nil
	}
	reader := bufio.NewReader(source)
	prefix, _ := reader.Peek(64)
	trimmedPrefix := strings.TrimSpace(string(prefix))
	var err error
	if strings.HasPrefix(trimmedPrefix, "data:") || strings.HasPrefix(trimmedPrefix, "event:") {
		err = consumeVideoSSE(reader, handle)
	} else {
		err = consumeVideoJSON(reader, handle)
	}
	if err != nil {
		return VideoResult{}, err
	}
	if result.URL == "" {
		return VideoResult{}, fmt.Errorf("视频生成完成但没有返回内容 URL")
	}
	return result, nil
}

func videoFileAttachments(root map[string]any) []string {
	modelResponse := nestedMap(root, "result", "response", "modelResponse")
	if modelResponse == nil {
		return nil
	}
	values, _ := modelResponse["fileAttachments"].([]any)
	attachments := make([]string, 0, len(values))
	for _, value := range values {
		if attachment, _ := value.(string); attachment != "" {
			attachments = append(attachments, attachment)
		}
	}
	return attachments
}

func setVideoResultURL(result *VideoResult, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	lower := strings.ToLower(value)
	path := strings.SplitN(lower, "?", 2)[0]
	if !strings.HasSuffix(path, ".mp4") && !strings.Contains(lower, "/content") {
		return false
	}
	result.URL = absoluteAssetURL(value)
	return true
}

func consumeVideoSSE(reader io.Reader, handle func(map[string]any) (bool, error)) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if line == "" || line == "[DONE]" || !strings.HasPrefix(line, "{") {
			continue
		}
		var root map[string]any
		if json.Unmarshal([]byte(line), &root) != nil {
			continue
		}
		complete, err := handle(root)
		if err != nil {
			return err
		}
		if complete {
			return nil
		}
	}
	return scanner.Err()
}

func consumeVideoJSON(reader io.Reader, handle func(map[string]any) (bool, error)) error {
	decoder := json.NewDecoder(io.LimitReader(reader, 64<<20))
	for {
		var root map[string]any
		if err := decoder.Decode(&root); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("解析视频上游流: %w", err)
		}
		complete, err := handle(root)
		if err != nil {
			return err
		}
		if complete {
			return nil
		}
	}
}

func nestedMap(value map[string]any, keys ...string) map[string]any {
	current := value
	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		if !ok {
			return nil
		}
		current = next
	}
	return current
}
