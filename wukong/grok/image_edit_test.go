package grok

import (
	"strings"
	"testing"
)

func TestBuildDirectFileUploadBodyIncludesImagineSource(t *testing.T) {
	body, contentType, err := buildDirectFileUploadBody(fileBytes{
		Filename: "ref.png", MIMEType: "image/png", Data: []byte("png"),
	}, imagineSelfUploadSource)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(contentType, "multipart/form-data") {
		t.Fatalf("contentType = %q", contentType)
	}
	if !strings.Contains(string(body), `name="file_source"`) || !strings.Contains(string(body), imagineSelfUploadSource) {
		t.Fatalf("file_source missing: %s", body)
	}
	chatBody, _, err := buildDirectFileUploadBody(fileBytes{
		Filename: "note.txt", MIMEType: "text/plain", Data: []byte("hi"),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(chatBody), "file_source") {
		t.Fatalf("chat upload should omit file_source: %s", chatBody)
	}
}

func TestConsumeUpstreamHandlesConcatenatedImageEditFrames(t *testing.T) {
	fixture := `{"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"users/test/generated/edit/image.jpg","progress":50}}}}` +
		`{"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"users/test/generated/edit/image.jpg","progress":100}}}}` +
		`{"result":{"response":{"modelResponse":{"generatedImageUrls":["users/test/generated/edit/image.jpg"]}}}}`
	var parsed parsedChat
	if err := consumeUpstreamInto(strings.NewReader(fixture), &parsed, nil); err != nil {
		t.Fatal(err)
	}
	want := "https://assets.grok.com/users/test/generated/edit/image.jpg"
	if len(parsed.Images) != 1 || parsed.Images[0] != want {
		t.Fatalf("images = %#v", parsed.Images)
	}
}

func TestStreamingImageEditRejectsModeratedFinalImage(t *testing.T) {
	parsed := &parsedChat{}
	frame := []byte(`{"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"users/test/generated/moderated/image.jpg","progress":100,"moderated":true}}}}`)
	kind, delta, err := parseUpstreamFrame(frame, parsed)
	if err != nil || kind != "" || delta != "" || len(parsed.Images) != 0 {
		t.Fatalf("kind=%q delta=%q images=%#v err=%v", kind, delta, parsed.Images, err)
	}
	final := []byte(`{"result":{"response":{"modelResponse":{"generatedImageUrls":["users/test/generated/moderated/image.jpg"]}}}}`)
	if kind, delta, err := parseUpstreamFrame(final, parsed); err != nil || kind != "" || delta != "" || len(parsed.Images) != 0 {
		t.Fatalf("fallback kind=%q delta=%q images=%#v err=%v", kind, delta, parsed.Images, err)
	}
	capture := append(append([]byte(nil), frame...), final...)
	if urls := imageEditResultURLs(parsed, capture); len(urls) != 0 {
		t.Fatalf("moderated capture leaked images: %#v", urls)
	}
}

func TestExtractCapturedImageURLsPrefersFinalImage(t *testing.T) {
	fixture := []byte(`{"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"users/test/generated/id-part-0/image.jpg","progress":50}}}}` +
		`{"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"users/test/generated/id/image.jpg","progress":100}}}}` +
		`{"result":{"response":{"modelResponse":{"generatedImageUrls":["users/test/generated/id/image.jpg"]}}}}`)
	got := extractCapturedImageURLs(fixture)
	if len(got) != 1 || got[0] != "https://assets.grok.com/users/test/generated/id/image.jpg" {
		t.Fatalf("urls = %#v", got)
	}
}

func TestHasImageAttachments(t *testing.T) {
	if hasImageAttachments(nil) || hasImageAttachments([]chatAttachmentInput{{Filename: "a.txt"}}) {
		t.Fatal("expected no image attachments")
	}
	if !hasImageAttachments([]chatAttachmentInput{{Source: "https://x/a.png", Image: true}}) {
		t.Fatal("expected image attachment")
	}
}
