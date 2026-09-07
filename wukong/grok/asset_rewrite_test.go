package grok

import (
	"context"
	"strings"
	"testing"
)

func TestAssetRewriterAppliedToTextLinks(t *testing.T) {
	c := &Client{}
	var seen []string
	c.SetAssetRewriter(func(_ context.Context, kind, raw string) string {
		seen = append(seen, kind+":"+raw)
		return "http://gw/files/" + kind + ".x"
	})
	in := "看图 ![a](https://assets.grok.com/users/u/generated/1/image.jpg) 和视频 https://assets.grok.com/users/u/generated/2/clip.mp4 完。"
	out := c.rewriteAssetLinksInText(context.Background(), in)
	if strings.Contains(out, "assets.grok.com") {
		t.Fatalf("raw asset link leaked: %s", out)
	}
	if len(seen) != 2 || !strings.HasPrefix(seen[0], "image:") || !strings.HasPrefix(seen[1], "video:") {
		t.Fatalf("rewriter calls = %v", seen)
	}
	// 非受信域名不改
	other := "https://example.com/a.jpg"
	if got := c.rewriteAssetLinksInText(context.Background(), other); got != other {
		t.Fatalf("untrusted host must be untouched: %s", got)
	}
}

func TestAssetRewriterNilIsNoop(t *testing.T) {
	c := &Client{}
	urls := []string{"https://assets.grok.com/x.jpg"}
	if got := c.rewriteAssets(context.Background(), AssetKindImage, urls); got[0] != urls[0] {
		t.Fatal("nil rewriter must return input")
	}
	if got := c.rewriteAsset(context.Background(), AssetKindVideo, "u"); got != "u" {
		t.Fatal("nil rewriter must return input")
	}
	// 钩子返回空串时保留原值，避免把链接吞掉
	c.SetAssetRewriter(func(context.Context, string, string) string { return "" })
	if got := c.rewriteAsset(context.Background(), AssetKindImage, "keep"); got != "keep" {
		t.Fatalf("empty rewrite must keep original, got %q", got)
	}
}

func TestVideoMarkdownIsPlainLink(t *testing.T) {
	if got := videoMarkdown("http://gw/files/v.mp4"); got != "[Generated Video](http://gw/files/v.mp4)" {
		t.Fatalf("videoMarkdown = %q", got)
	}
}
