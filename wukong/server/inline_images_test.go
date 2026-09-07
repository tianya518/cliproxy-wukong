package server

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	sentinel "github.com/router-for-me/CLIProxyAPI/v7/wukong/sentinel"
)

func seedImage(t *testing.T, s *ArtifactStore, id string, data []byte) {
	t.Helper()
	if err := s.Record(ArtifactRecord{ID: id, Ext: "png", Provider: ArtifactProviderChatGPT,
		Locator: ArtifactLocator{ConvID: "c", FileID: id}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBytes(id, data, "image/png"); err != nil {
		t.Fatal(err)
	}
}

func TestInlineImagePartsBuildsDataURLs(t *testing.T) {
	s := newTestStore(t, ArtifactStoreOptions{})
	seedImage(t, s, "img_a", []byte("AAA"))
	seedImage(t, s, "img_b", []byte("BBBB"))
	// 非图片产物不内联
	_ = s.Record(ArtifactRecord{ID: "doc", Ext: "pdf", Provider: ArtifactProviderChatGPT})
	_ = s.PutBytes("doc", []byte("%PDF"), "application/pdf")

	parts := s.InlineImageParts(context.Background(), []string{"img_a", "doc", "img_b", "img_a", "missing"})
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2 (dedup, skip non-image and unknown)", len(parts))
	}
	if parts[0].Type != "image_url" || parts[0].Index != 0 || parts[1].Index != 1 {
		t.Fatalf("parts = %+v", parts)
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("AAA"))
	if parts[0].ImageURL.URL != want {
		t.Fatalf("url = %q", parts[0].ImageURL.URL)
	}
}

func TestInlineImagePartsRespectsSwitchAndLimit(t *testing.T) {
	s := newTestStore(t, ArtifactStoreOptions{})
	seedImage(t, s, "big", []byte(strings.Repeat("x", 100)))
	seedImage(t, s, "small", []byte("ok"))

	s.SetInlineImages(true, 50)
	parts := s.InlineImageParts(context.Background(), []string{"big", "small"})
	if len(parts) != 1 || !strings.HasSuffix(parts[0].ImageURL.URL, base64.StdEncoding.EncodeToString([]byte("ok"))) {
		t.Fatalf("size limit not applied: %+v", parts)
	}

	s.SetInlineImages(false, 0)
	if parts := s.InlineImageParts(context.Background(), []string{"small"}); parts != nil {
		t.Fatalf("disabled inline must return nil, got %+v", parts)
	}
}

func TestIDFromPublicURL(t *testing.T) {
	s := newTestStore(t, ArtifactStoreOptions{})
	seedImage(t, s, "known", []byte("k"))
	cases := map[string]string{
		"http://23.142.200.35:8317/files/known.png":       "known",
		"https://gw.example.com/files/known.png?x=1#frag": "known", // 换了域名的旧链接也认
		"/files/known.png":                        "known",
		"http://gw/files/unknown.png":             "",
		"http://gw/images/known.png":              "",
		"http://gw/files/../known.png":            "",
		"https://assets.grok.com/files/known.png": "known", // 只看路径不看主机
		"":          "",
		"not a url": "",
	}
	for in, want := range cases {
		got, ok := s.IDFromPublicURL(in)
		if (want == "") == ok || got != want {
			t.Errorf("IDFromPublicURL(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
}

func TestReattachHistoryImagesReplacesLinksAndLimits(t *testing.T) {
	cfg := &ServerConfig{SessionTTLMinutes: 1, ImageDir: t.TempDir(), ArtifactHistoryReattach: 2}
	s := newTestStore(t, ArtifactStoreOptions{})
	for _, id := range []string{"one", "two", "three"} {
		seedImage(t, s, id, []byte(id))
	}
	e := NewEngine(cfg, nil, NewSessionManager(cfg))
	e.SetArtifactStore(s)

	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("inline"))
	msgs := []Message{
		{Role: "user", Content: "画三张猫"},
		{Role: "assistant", Content: "好\n\n![Generated Image 1](http://gw/files/one.png)\n\n![Generated Image 2](http://gw/files/two.png)\n\n![外部](https://example.com/x.png)"},
		{Role: "user", Content: "再来一张"},
		{Role: "assistant", Content: "![Generated Image](http://gw/files/three.png)", Images: []ImagePart{{Type: "image_url", ImageURL: ImageURL{URL: dataURL}}}},
		{Role: "user", Content: "把第三张换成狗"},
	}
	history := flattenHistory(msgs)
	out, sources, note := e.reattachHistoryImages(msgs, history)

	// 4 张候选（one two three + data URL），上限 2 → 只挂最近两张：three 与 data URL
	if len(sources) != 2 || sources[0] != artifactRefScheme+"three" || sources[1] != dataURL {
		t.Fatalf("sources = %v", sources)
	}
	if strings.Contains(out, "/files/one.png") || strings.Contains(out, "/files/two.png") || strings.Contains(out, "/files/three.png") {
		t.Fatalf("own links must be replaced:\n%s", out)
	}
	if !strings.Contains(out, "[图片]") || !strings.Contains(out, "[图片 1]") {
		t.Fatalf("placeholders missing:\n%s", out)
	}
	if strings.Contains(out, "[图片 2]") {
		t.Fatalf("data URL image has no markdown to replace, must not consume a text placeholder:\n%s", out)
	}
	if !strings.Contains(out, "https://example.com/x.png") {
		t.Fatalf("foreign links must stay untouched:\n%s", out)
	}
	if !strings.Contains(note, "2 张") {
		t.Fatalf("note = %q", note)
	}

	// 关闭回挂
	cfg.ArtifactHistoryReattach = 0
	out2, sources2, note2 := e.reattachHistoryImages(msgs, history)
	if out2 != history || sources2 != nil || note2 != "" {
		t.Fatal("disabled reattach must be a no-op")
	}
}

func TestReattachHistoryImagesNoOpWithoutOwnImages(t *testing.T) {
	cfg := &ServerConfig{SessionTTLMinutes: 1, ImageDir: t.TempDir(), ArtifactHistoryReattach: 4}
	e := NewEngine(cfg, nil, NewSessionManager(cfg))
	e.SetArtifactStore(newTestStore(t, ArtifactStoreOptions{}))
	msgs := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "see ![x](https://example.com/a.png)"},
		{Role: "user", Content: "ok"},
	}
	history := flattenHistory(msgs)
	out, sources, note := e.reattachHistoryImages(msgs, history)
	if out != history || sources != nil || note != "" {
		t.Fatalf("unexpected change: out=%q sources=%v note=%q", out, sources, note)
	}
}

func TestImagesToReturnFiltersOnContinuation(t *testing.T) {
	cfg := &ServerConfig{SessionTTLMinutes: 1, ImageDir: t.TempDir()}
	e := NewEngine(cfg, nil, NewSessionManager(cfg))
	entry := e.session.GetOrCreate("", "tok")
	result := &sentinel.ChatResult{ExpectGeneratedImages: true, ImageFileIDs: []string{"a", "b", "c", "d"}}

	// 新会话：全给
	if got := e.imagesToReturn(entry, ChatCompletionRequest{}, result); len(got) != 4 {
		t.Fatalf("fresh conversation should return all, got %v", got)
	}
	entry.markReturned(result.ImageFileIDs)

	// 续接：又出了一张 e，旧的 a-d 不再返回
	result2 := &sentinel.ChatResult{ExpectGeneratedImages: true, ImageFileIDs: []string{"c", "a", "e", "b", "d"}}
	got := e.imagesToReturn(entry, ChatCompletionRequest{ConversationID: "conv"}, result2)
	if len(got) != 1 || got[0] != "e" {
		t.Fatalf("continuation should only return new ids, got %v", got)
	}

	// 续接但没有新图：markdown 不再贴旧图链接
	s := newTestStore(t, ArtifactStoreOptions{})
	e.SetArtifactStore(s)
	md := e.artifactMarkdown(ChatEnv{AbsoluteURL: testAbsolute}, entry, ChatCompletionRequest{ConversationID: "conv"}, result, "conv", nil)
	if strings.Contains(md, "![") {
		t.Fatalf("no new images → no image markdown, got %q", md)
	}
	// 单张新图用不带编号的标题
	md = e.artifactMarkdown(ChatEnv{AbsoluteURL: testAbsolute}, entry, ChatCompletionRequest{ConversationID: "conv"}, result2, "conv", []string{"e"})
	if !strings.Contains(md, "![Generated Image](http://gw.example/files/e.png)") || strings.Contains(md, "Image 1") {
		t.Fatalf("md = %q", md)
	}
}
