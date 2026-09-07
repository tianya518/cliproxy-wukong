package cliproxy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/wukong/grok"
	sentinelserver "github.com/router-for-me/CLIProxyAPI/v7/wukong/server"
)

func TestGrokExecutorAttachInlineImages(t *testing.T) {
	store, err := sentinelserver.NewArtifactStore(sentinelserver.ArtifactStoreOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Record(sentinelserver.ArtifactRecord{ID: "abc", Ext: "jpg", Provider: sentinelserver.ArtifactProviderGrok,
		Locator: sentinelserver.ArtifactLocator{URL: "https://assets.grok.com/x/abc.jpg"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutBytes("abc", []byte("JPG"), "image/jpeg"); err != nil {
		t.Fatal(err)
	}

	x := NewGrokExecutor(grok.Config{})
	x.SetArtifactStore(store, func(p string) string { return "http://gw" + p }, 0)

	result := &grok.ChatResult{ID: "id", Model: "grok-imagine-image", Text: "![Generated Image 1](http://gw/files/abc.jpg)",
		Images: []string{"http://gw/files/abc.jpg", "https://example.com/not-ours.jpg"}, FinishReason: "stop"}
	completion := grok.OpenAICompletion(result)
	x.attachInlineImages(context.Background(), completion, "message", result.Images)

	raw, _ := json.Marshal(completion)
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				Images  []struct {
					Type     string `json:"type"`
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"images"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	imgs := parsed.Choices[0].Message.Images
	if len(imgs) != 1 || imgs[0].Type != "image_url" || !strings.HasPrefix(imgs[0].ImageURL.URL, "data:image/jpeg;base64,") {
		t.Fatalf("images = %+v", imgs)
	}
	if !strings.Contains(parsed.Choices[0].Message.Content, "/files/abc.jpg") {
		t.Fatal("markdown link must remain alongside inline images")
	}

	// delta 槽位（流式收尾 chunk）
	stop := "stop"
	chunk := grok.OpenAIChunk("id", "grok-imagine-image", "", "", &stop, "")
	x.attachInlineImages(context.Background(), chunk, "delta", result.Images)
	raw, _ = json.Marshal(chunk)
	if !strings.Contains(string(raw), `"images":[{"type":"image_url"`) {
		t.Fatalf("delta.images missing: %s", raw)
	}

	// 没挂存储：不加字段
	plain := NewGrokExecutor(grok.Config{})
	c2 := grok.OpenAICompletion(result)
	plain.attachInlineImages(context.Background(), c2, "message", result.Images)
	raw, _ = json.Marshal(c2)
	if strings.Contains(string(raw), `"images"`) {
		t.Fatal("no store → no images field")
	}
}
