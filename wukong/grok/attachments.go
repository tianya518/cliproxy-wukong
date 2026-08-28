package grok

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	errInvalidChatAttachment = errors.New("对话附件无效")
	errInvalidChatImage      = errors.New("对话图片无效")
	errInvalidChatFile       = errors.New("对话文件无效")
)

func (c *Client) prepareChatAttachments(ctx context.Context, inputs []chatAttachmentInput) ([]string, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	if len(inputs) > maxChatAttachments {
		return nil, fmt.Errorf("%w: 单次对话最多支持 %d 个附件", errInvalidChatAttachment, maxChatAttachments)
	}
	pending := make([]fileBytes, 0, len(inputs))
	seen := make(map[string]struct{}, len(inputs))
	total := int64(0)
	for _, input := range inputs {
		input.Source = strings.TrimSpace(input.Source)
		key := fmt.Sprintf("%t\x00%s\x00%s", input.Image, input.Filename, input.Source)
		if _, exists := seen[key]; exists {
			continue
		}
		var file fileBytes
		var err error
		if input.Image {
			file, err = c.loadChatImage(ctx, input.Source)
		} else {
			file, err = c.loadChatFile(ctx, input.Source, input.Filename)
		}
		if err != nil {
			return nil, err
		}
		size := int64(len(file.Data))
		if size > maxChatAttachmentTotal || total > maxChatAttachmentTotal-size {
			return nil, fmt.Errorf("%w: 总大小不能超过 64 MiB", errInvalidChatAttachment)
		}
		total += size
		seen[key] = struct{}{}
		pending = append(pending, file)
	}
	attachments := make([]string, 0, len(pending))
	for _, file := range pending {
		uploaded, err := c.uploadFileV2Direct(ctx, file, c.baseURL()+"/", "chat_attachment_upload")
		if err != nil {
			return nil, err
		}
		if uploaded.ID == "" {
			return nil, fmt.Errorf("上传附件成功但上游未返回可用附件标识")
		}
		attachments = append(attachments, uploaded.ID)
	}
	return attachments, nil
}

func (c *Client) loadChatImage(ctx context.Context, input string) (fileBytes, error) {
	if strings.HasPrefix(strings.ToLower(input), "data:") {
		return parseDataURI(input, "", c.cfg.MaxInputImageBytes, true)
	}
	return c.downloadAttachment(ctx, input, "", true)
}

func (c *Client) loadChatFile(ctx context.Context, input, filename string) (fileBytes, error) {
	if strings.HasPrefix(strings.ToLower(input), "data:") {
		return parseDataURI(input, filename, c.cfg.MaxInputImageBytes, false)
	}
	return c.downloadAttachment(ctx, input, filename, false)
}

func (c *Client) downloadAttachment(ctx context.Context, rawURL, filename string, image bool) (fileBytes, error) {
	if len(rawURL) > maxRemoteAttachmentURLLen {
		return fileBytes{}, errInvalidChatAttachment
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return fileBytes{}, errInvalidChatAttachment
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	headers := http.Header{}
	if image {
		headers.Set("Accept", "image/webp,image/png,image/jpeg,image/gif;q=0.9,*/*;q=0.1")
	} else {
		headers.Set("Accept", "application/pdf,text/*,application/json,application/xml,application/rtf,application/msword,application/zip,image/*,*/*;q=0.1")
	}
	headers.Set("User-Agent", c.userAgent())
	resp, err := c.do(reqCtx, http.MethodGet, rawURL, nil, headers)
	if err != nil {
		return fileBytes{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if image {
			return fileBytes{}, fmt.Errorf("%w: 下载地址返回 %d", errInvalidChatImage, resp.StatusCode)
		}
		return fileBytes{}, fmt.Errorf("%w: 下载地址返回 %d", errInvalidChatFile, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxInputImageBytes+1))
	if err != nil || int64(len(raw)) > c.cfg.MaxInputImageBytes {
		return fileBytes{}, errInvalidChatAttachment
	}
	mimeType := resp.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = http.DetectContentType(raw)
	}
	if filename == "" {
		filename = path.Base(parsed.Path)
	}
	if filename == "" || filename == "." || filename == "/" {
		filename = "upload.bin"
	}
	return fileBytes{Filename: filename, MIMEType: mimeType, Data: raw}, nil
}

func parseDataURI(value, filename string, maxBytes int64, image bool) (fileBytes, error) {
	comma := strings.Index(value, ",")
	if comma < 0 || !strings.HasPrefix(strings.ToLower(value), "data:") {
		return fileBytes{}, errInvalidChatAttachment
	}
	header := value[5:comma]
	payload := value[comma+1:]
	var data []byte
	var err error
	if strings.Contains(header, ";base64") {
		data, err = base64.StdEncoding.DecodeString(payload)
	} else {
		data = []byte(payload)
	}
	if err != nil || int64(len(data)) > maxBytes {
		return fileBytes{}, errInvalidChatAttachment
	}
	mimeType := strings.TrimSuffix(header, ";base64")
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	if filename == "" {
		filename = "upload.bin"
		if ext, _ := mime.ExtensionsByType(strings.Split(mimeType, ";")[0]); len(ext) > 0 {
			filename = "upload" + ext[0]
		}
	}
	_ = image
	return fileBytes{Filename: filename, MIMEType: mimeType, Data: data}, nil
}

func (c *Client) uploadFileV2Direct(ctx context.Context, file fileBytes, referer, fileSource string) (uploadedFile, error) {
	body, contentType, err := buildDirectFileUploadBody(file, fileSource)
	if err != nil {
		return uploadedFile{}, err
	}
	endpoint := c.baseURL() + "/http/upload-file-v2/direct"
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		reqCtx, cancel := context.WithTimeout(ctx, time.Minute)
		headers := c.restHeaders(contentType, referer)
		headers.Del("x-xai-request-id")
		if signErr := c.applySignedStatsig(reqCtx, headers, http.MethodPost, endpoint); signErr != nil {
			cancel()
			return uploadedFile{}, signErr
		}
		resp, err := c.do(reqCtx, http.MethodPost, endpoint, body, headers)
		if err != nil {
			cancel()
			return uploadedFile{}, err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			uploaded, decodeErr := decodeDirectFileUploadResponse(io.LimitReader(resp.Body, directFileUploadResponseLimit))
			_ = resp.Body.Close()
			cancel()
			return uploaded, decodeErr
		}
		mediaErr := readMediaStatusError("V2 上传文件", resp)
		cancel()
		lastErr = mediaErr
		if attempt == 0 && isPageOutOfDate(mediaErr.status, mediaErr.body) {
			c.statsig.invalidate()
			continue
		}
		return uploadedFile{}, mediaErr
	}
	return uploadedFile{}, lastErr
}

func buildDirectFileUploadBody(file fileBytes, fileSource string) ([]byte, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, browserMultipartFilename(file.Filename)))
	header.Set("Content-Type", file.MIMEType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(file.Data); err != nil {
		return nil, "", err
	}
	if fileSource != "" {
		if err := writer.WriteField("file_source", fileSource); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func browserMultipartFilename(value string) string {
	value = strings.Map(func(character rune) rune {
		switch {
		case character == '\r' || character == '\n':
			return -1
		case character < 0x20 || character == 0x7f:
			return '_'
		default:
			return character
		}
	}, value)
	if strings.TrimSpace(value) == "" {
		value = "upload.bin"
	}
	if !utf8.ValidString(value) {
		value = "upload.bin"
	}
	return strings.NewReplacer("\\", "\\\\", `"`, `\"`).Replace(value)
}

func decodeDirectFileUploadResponse(source io.Reader) (uploadedFile, error) {
	var value struct {
		UploadID      string          `json:"uploadId"`
		TerminalError json.RawMessage `json:"terminalError"`
		FileMetadata  struct {
			ID      string `json:"fileMetadataId"`
			FileID  string `json:"fileId"`
			FileURI string `json:"fileUri"`
		} `json:"fileMetadata"`
	}
	if err := json.NewDecoder(source).Decode(&value); err != nil {
		return uploadedFile{}, fmt.Errorf("V2 上传文件响应无效: %w", err)
	}
	if len(bytes.TrimSpace(value.TerminalError)) > 0 && !bytes.Equal(bytes.TrimSpace(value.TerminalError), []byte("null")) && !bytes.Equal(bytes.TrimSpace(value.TerminalError), []byte("false")) {
		return uploadedFile{}, errors.New("V2 上传文件被上游拒绝")
	}
	metadataID := strings.TrimSpace(value.FileMetadata.ID)
	fileID := metadataID
	if fileID == "" {
		fileID = strings.TrimSpace(value.FileMetadata.FileID)
	}
	if fileID == "" {
		fileID = strings.TrimSpace(value.UploadID)
	}
	fileURI := ""
	if value.FileMetadata.FileURI != "" {
		fileURI = absoluteAssetURL(value.FileMetadata.FileURI)
	}
	if fileID == "" && fileURI == "" {
		return uploadedFile{}, fmt.Errorf("V2 上传文件成功但上游未返回完整文件标识")
	}
	return uploadedFile{ID: fileID, MetadataID: metadataID, URI: fileURI}, nil
}
