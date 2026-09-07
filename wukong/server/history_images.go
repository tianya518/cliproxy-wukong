package server

// history_images.go —— 历史图片回挂。
//
// 标准 OpenAI 客户端不回传 conversation_id，每轮都是新的官网会话，历史只能展平成文本。
// 上一轮 assistant 消息里的生图在文本里只是一行 URL，上游模型打不开。这里把历史中本网关
// 生成过的图（/files/<id> 链接，或客户端原样回传的 images[]）重新作为附件挂进本轮，并把
// 文本里的链接换成 [图片 N] 占位，让模型知道附件和历史的对应关系。

import (
	"fmt"
	"regexp"
	"strings"
)

// artifactRefScheme 标记"从产物存储里读"的附件引用：artifact:<id>。
const artifactRefScheme = "artifact:"

var markdownImageRe = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]+)\)`)

type historyImageRef struct {
	match string // 历史文本里的 markdown 片段，用于替换成占位；images[] 来源时为空
	src   string // artifact:<id> 或 data URL
}

// collectHistoryImageRefs 扫最后一条 user 之前的 assistant 消息，按出现顺序收集本网关生成过的图。
func (e *Engine) collectHistoryImageRefs(messages []Message) []historyImageRef {
	if e == nil || e.store == nil {
		return nil
	}
	last := lastUserIndex(messages)
	if last <= 0 {
		return nil
	}
	var refs []historyImageRef
	seen := make(map[string]bool)
	add := func(r historyImageRef) {
		if r.src == "" || seen[r.src] {
			return
		}
		seen[r.src] = true
		refs = append(refs, r)
	}
	for _, m := range messages[:last] {
		if strings.ToLower(m.Role) != "assistant" {
			continue
		}
		text, _ := parseMessageContent(m.Content)
		for _, sm := range markdownImageRe.FindAllStringSubmatch(text, -1) {
			if id, ok := e.store.IDFromPublicURL(sm[2]); ok {
				add(historyImageRef{match: sm[0], src: artifactRefScheme + id})
			}
		}
		for _, img := range m.Images {
			u := strings.TrimSpace(img.ImageURL.URL)
			switch {
			case strings.HasPrefix(u, "data:image/"):
				add(historyImageRef{src: u})
			default:
				if id, ok := e.store.IDFromPublicURL(u); ok {
					add(historyImageRef{src: artifactRefScheme + id})
				}
			}
		}
	}
	return refs
}

// reattachHistoryImages 返回：替换了占位符的历史文本、要重新上传的附件引用（最近 N 张）、
// 给模型看的说明行。没有可回挂的图时原样返回。
func (e *Engine) reattachHistoryImages(messages []Message, history string) (string, []string, string) {
	if e == nil || e.store == nil || e.cfg == nil || e.cfg.ArtifactHistoryReattach <= 0 || history == "" {
		return history, nil, ""
	}
	refs := e.collectHistoryImageRefs(messages)
	if len(refs) == 0 {
		return history, nil, ""
	}
	max := e.cfg.ArtifactHistoryReattach
	attachedFrom := 0
	if len(refs) > max {
		attachedFrom = len(refs) - max
	}
	out := history
	sources := make([]string, 0, max)
	n := 0
	for i, r := range refs {
		label := "[图片]"
		if i >= attachedFrom {
			n++
			label = fmt.Sprintf("[图片 %d]", n)
			sources = append(sources, r.src)
		}
		if r.match != "" {
			out = strings.Replace(out, r.match, label, 1)
		}
	}
	if n == 0 {
		return history, nil, ""
	}
	note := fmt.Sprintf("[Attached images: 本轮附带的 %d 张图片就是上文标记为 [图片 1]…[图片 %d] 的、此前对话中生成的图，按顺序一一对应。请把它们当作上下文；除非用户要求，不要原样重新生成。]", n, n)
	return out, sources, note
}
