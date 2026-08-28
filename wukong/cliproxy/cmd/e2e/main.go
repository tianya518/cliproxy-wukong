// Command e2e exercises the wukong-gateway gateway across all three inbound
// protocols, verifying that cliproxy's built-in translators correctly convert
// Claude and Gemini requests into the OpenAI format our executor declares.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

var (
	base  = env("GW", "http://127.0.0.1:8317")
	key   = env("GW_KEY", "sk-local-test")
	model = env("GW_MODEL", "gpt-5-6-instant")
)

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

var failures int

func check(name string, ok bool, detail string) {
	status := "PASS"
	if !ok {
		status = "FAIL"
		failures++
	}
	fmt.Printf("  [%s] %-24s %s\n", status, name, detail)
}

func short(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}

func do(method, path string, body any, hdr map[string]string) (*http.Response, error) {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return (&http.Client{Timeout: 5 * time.Minute}).Do(req)
}

func bearer() map[string]string { return map[string]string{"Authorization": "Bearer " + key} }

// modelsList checks that our provider's models are advertised.
func modelsList() {
	resp, err := do(http.MethodGet, "/v1/models", nil, bearer())
	if err != nil {
		check("models list", false, err.Error())
		return
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	has := false
	for _, id := range ids {
		if id == model {
			has = true
		}
	}
	check("models list", has && len(ids) > 0,
		fmt.Sprintf("count=%d has %s=%v [%s]", len(ids), model, has, short(strings.Join(ids, ","))))
}

// openaiNonStream drives the native OpenAI protocol.
func openaiNonStream() {
	resp, err := do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "Reply with exactly: BETA"}},
	}, bearer())
	if err != nil {
		check("openai non-stream", false, err.Error())
		return
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error any `json:"error"`
	}
	raw, _ := readAll(resp)
	json.Unmarshal(raw, &out)
	txt := ""
	if len(out.Choices) > 0 {
		txt = out.Choices[0].Message.Content
	}
	check("openai non-stream", strings.Contains(strings.ToUpper(txt), "BETA"),
		fmt.Sprintf("status=%d text=%q", resp.StatusCode, short(orRaw(txt, raw))))
}

// openaiStream verifies SSE framing survives the executor boundary.
func openaiStream() {
	resp, err := do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    model,
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "List numbers 1 to 20 separated by commas."}},
	}, bearer())
	if err != nil {
		check("openai stream", false, err.Error())
		return
	}
	defer resp.Body.Close()

	var content strings.Builder
	chunks, done := 0, false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<22)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		p := strings.TrimPrefix(line, "data: ")
		if p == "[DONE]" {
			done = true
			break
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(p), &c) != nil {
			continue
		}
		for _, ch := range c.Choices {
			if ch.Delta.Content != "" {
				content.WriteString(ch.Delta.Content)
				chunks++
			}
		}
	}
	check("openai stream", done && chunks > 0 && strings.Contains(content.String(), "20"),
		fmt.Sprintf("chunks=%d done=%v text=%q", chunks, done, short(content.String())))
}

// claudeMessages goes through cliproxy's Claude->OpenAI translator.
func claudeMessages() {
	resp, err := do(http.MethodPost, "/v1/messages", map[string]any{
		"model":      model,
		"max_tokens": 64,
		"messages":   []map[string]string{{"role": "user", "content": "Reply with exactly: GAMMA"}},
	}, map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
	if err != nil {
		check("claude /v1/messages", false, err.Error())
		return
	}
	defer resp.Body.Close()
	raw, _ := readAll(resp)
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	json.Unmarshal(raw, &out)
	txt := ""
	for _, c := range out.Content {
		if c.Type == "text" {
			txt += c.Text
		}
	}
	check("claude /v1/messages", strings.Contains(strings.ToUpper(txt), "GAMMA"),
		fmt.Sprintf("status=%d text=%q", resp.StatusCode, short(orRaw(txt, raw))))
}

// geminiGenerate goes through cliproxy's Gemini->OpenAI translator.
func geminiGenerate() {
	path := "/v1beta/models/" + model + ":generateContent"
	resp, err := do(http.MethodPost, path, map[string]any{
		"contents": []map[string]any{{
			"role":  "user",
			"parts": []map[string]string{{"text": "Reply with exactly: DELTA"}},
		}},
	}, map[string]string{"x-goog-api-key": key})
	if err != nil {
		check("gemini generateContent", false, err.Error())
		return
	}
	defer resp.Body.Close()
	raw, _ := readAll(resp)
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	json.Unmarshal(raw, &out)
	txt := ""
	for _, c := range out.Candidates {
		for _, p := range c.Content.Parts {
			txt += p.Text
		}
	}
	check("gemini generateContent", strings.Contains(strings.ToUpper(txt), "DELTA"),
		fmt.Sprintf("status=%d text=%q", resp.StatusCode, short(orRaw(txt, raw))))
}

// claudeStream verifies the stateful Claude SSE event sequence survives
// translation: a single shared translator state must produce message_start,
// content_block_delta frames and message_stop in order.
func claudeStream() {
	resp, err := do(http.MethodPost, "/v1/messages", map[string]any{
		"model":      model,
		"max_tokens": 256,
		"stream":     true,
		"messages":   []map[string]string{{"role": "user", "content": "List numbers 1 to 20 separated by commas."}},
	}, map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
	if err != nil {
		check("claude stream", false, err.Error())
		return
	}
	defer resp.Body.Close()

	var text strings.Builder
	events := map[string]int{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<22)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "event: ") {
			events[strings.TrimPrefix(line, "event: ")]++
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		if ev.Type == "content_block_delta" {
			text.WriteString(ev.Delta.Text)
		}
	}
	ok := events["message_start"] == 1 && events["content_block_delta"] > 0 &&
		events["message_stop"] == 1 && strings.Contains(text.String(), "20")
	check("claude stream", ok, fmt.Sprintf("start=%d delta=%d stop=%d text=%q",
		events["message_start"], events["content_block_delta"], events["message_stop"], short(text.String())))
}

// geminiStream checks the Gemini streaming path end to end.
func geminiStream() {
	path := "/v1beta/models/" + model + ":streamGenerateContent?alt=sse"
	resp, err := do(http.MethodPost, path, map[string]any{
		"contents": []map[string]any{{
			"role":  "user",
			"parts": []map[string]string{{"text": "List numbers 1 to 20 separated by commas."}},
		}},
	}, map[string]string{"x-goog-api-key": key})
	if err != nil {
		check("gemini stream", false, err.Error())
		return
	}
	defer resp.Body.Close()

	var text strings.Builder
	frames := 0
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<22)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		frames++
		for _, c := range ev.Candidates {
			for _, p := range c.Content.Parts {
				text.WriteString(p.Text)
			}
		}
	}
	check("gemini stream", frames > 0 && strings.Contains(text.String(), "20"),
		fmt.Sprintf("frames=%d text=%q", frames, short(text.String())))
}

// imageGen confirms generated images surface as reachable markdown links.
func imageGen() {
	resp, err := do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "dall-e-3",
		"messages": []map[string]string{{"role": "user", "content": "A single blue square on white."}},
	}, bearer())
	if err != nil {
		check("image gen", false, err.Error())
		return
	}
	defer resp.Body.Close()
	raw, _ := readAll(resp)
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	json.Unmarshal(raw, &out)
	txt := ""
	if len(out.Choices) > 0 {
		txt = out.Choices[0].Message.Content
	}
	url := markdownURL(txt)
	if url == "" {
		check("image gen", false, fmt.Sprintf("status=%d no markdown link in %q", resp.StatusCode, short(orRaw(txt, raw))))
		return
	}
	// 链接可达才算数：产物由 wukong 自己的路由提供，网关不认识
	// /api/image/proxy，ARTIFACT_BASE_URL 配错的话这里就会失败。
	imgResp, err := (&http.Client{Timeout: time.Minute}).Get(url)
	if err != nil {
		check("image gen", false, "fetch "+url+": "+err.Error())
		return
	}
	defer imgResp.Body.Close()
	body, _ := readAll(imgResp)
	ct := imgResp.Header.Get("Content-Type")
	check("image gen", imgResp.StatusCode == 200 && len(body) > 1024,
		fmt.Sprintf("url=%s status=%d type=%s bytes=%d", short(url), imgResp.StatusCode, ct, len(body)))
}

// markdownURL pulls the target out of the first ![...](url) link.
func markdownURL(s string) string {
	i := strings.Index(s, "](")
	if i < 0 {
		return ""
	}
	rest := s[i+2:]
	j := strings.Index(rest, ")")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func readAll(resp *http.Response) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(resp.Body)
	return buf.Bytes(), err
}

func orRaw(txt string, raw []byte) string {
	if strings.TrimSpace(txt) != "" {
		return txt
	}
	return string(raw)
}

func main() {
	fmt.Printf("gateway=%s model=%s\n\n", base, model)
	modelsList()
	openaiNonStream()
	openaiStream()
	claudeMessages()
	claudeStream()
	geminiGenerate()
	geminiStream()
	if os.Getenv("SKIP_IMAGE") == "" {
		imageGen()
	}
	fmt.Println()
	if failures > 0 {
		fmt.Printf("%d check(s) failed\n", failures)
		os.Exit(1)
	}
	fmt.Println("all checks passed")
}
