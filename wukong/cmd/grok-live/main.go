// grok-live 用本地 grok.json 真打 grok.com：session / 额度 / 聊天 / 生图 / 视频。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/wukong/grok"
)

func main() {
	file := flag.String("file", "grok.json", "Grok 账号文件")
	skipMedia := flag.Bool("skip-media", false, "只测聊天，不测生图/视频")
	skipVideo := flag.Bool("skip-video", false, "测聊天和图片，不测视频")
	onlyRefs := flag.Bool("only-refs", false, "只测多图参考生视频")
	statsigTrace := flag.Bool("statsig-trace", false, "只对照本地 Statsig 材料")
	statsigLocal := flag.Bool("statsig-local", false, "只用本地 Statsig，不走远程签名")
	flag.Parse()

	accounts, err := grok.LoadCredentialsFile(*file)
	if err != nil || len(accounts) == 0 {
		fmt.Fprintf(os.Stderr, "读 %s 失败: %v\n", *file, err)
		os.Exit(1)
	}
	cfg := grok.ConfigFromEnv()
	if *statsigLocal {
		cfg.StatsigMode = grok.StatsigModeLocal
	}
	cfg.OnClearanceUpdate = func(cred grok.Credential) {
		for i := range accounts {
			if accounts[i].AccessToken() == cred.AccessToken() {
				accounts[i].CloudflareCookies = cred.CloudflareCookies
				accounts[i].UserAgent = cred.UserAgent
			}
		}
		_ = grok.SaveCredentialsFile(*file, accounts)
	}
	client := grok.NewClient(cfg, accounts[0])
	ctx := context.Background()
	failed := 0

	run := func(name string, timeout time.Duration, fn func(context.Context) error) {
		fmt.Printf("\n== %s ==\n", name)
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		start := time.Now()
		if err := fn(reqCtx); err != nil {
			failed++
			fmt.Printf("FAIL %s (%s): %v\n", name, time.Since(start).Round(time.Millisecond), err)
			return
		}
		fmt.Printf("PASS %s (%s)\n", name, time.Since(start).Round(time.Millisecond))
	}

	run("session", 20*time.Second, func(ctx context.Context) error {
		id, err := client.FetchSession(ctx)
		if err != nil {
			return err
		}
		if id.UserID == "" {
			return fmt.Errorf("session 没有 user_id")
		}
		fmt.Printf("user_id=%s\n", id.UserID)
		return nil
	})

	statsigTimeout := 60 * time.Second
	if *statsigLocal {
		statsigTimeout = 3 * time.Minute
	}
	run("statsig", statsigTimeout, func(ctx context.Context) error {
		inspect, err := client.InspectStatsig(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("page=%s source=%s local=%v scripts=%d indices=%s (%s) key=%s hash16=%s counter=%d\n",
			inspect.PagePath, inspect.Source, inspect.Local, inspect.Scripts, inspect.Indices, inspect.IndexSource, inspect.AnimationKey, inspect.Hash16, inspect.Counter)
		if inspect.LocalError != "" {
			fmt.Printf("local_error=%s\n", inspect.LocalError)
		}
		if *statsigTrace || *statsigLocal {
			scan, scanErr := client.ScanStatsigCurveHints(ctx)
			if scanErr != nil {
				fmt.Printf("scan_error=%v\n", scanErr)
			} else {
				fmt.Printf("scan scripts=%d curves=%d svg=%d anim=%d html=%d flight=%d next_f=%v botox=%v escaped=%v title=%q\n",
					scan.Scripts, scan.Curves, scan.SVG, scan.Anim, scan.HTMLBytes, scan.FlightBytes, scan.HasNextFlight, scan.HasBotoxFooter, scan.HasEscapedJSON, scan.Title)
				for _, hit := range scan.Hits {
					fmt.Printf("  hit %s\n", hit)
				}
			}
		}
		if *statsigLocal && !inspect.Local {
			return fmt.Errorf("本地 Statsig 不可用")
		}
		return nil
	})

	if *statsigTrace {
		exit(failed)
	}
	if *statsigLocal {
		run("quota", 30*time.Second, func(ctx context.Context) error {
			snap, err := client.SyncQuota(ctx)
			if err != nil {
				return err
			}
			for _, w := range snap.Windows {
				avail := ""
				if w.Available != nil {
					avail = fmt.Sprintf(" available=%v", *w.Available)
				}
				fmt.Printf("  %s remaining=%d total=%d%s\n", w.Mode, w.Remaining, w.Total, avail)
			}
			return nil
		})
		exit(failed)
	}

	run("quota", 30*time.Second, func(ctx context.Context) error {
		snap, err := client.SyncQuota(ctx)
		if err != nil {
			return err
		}
		for _, w := range snap.Windows {
			avail := ""
			if w.Available != nil {
				avail = fmt.Sprintf(" available=%v", *w.Available)
			}
			fmt.Printf("  %s remaining=%d total=%d%s\n", w.Mode, w.Remaining, w.Total, avail)
		}
		return nil
	})

	run("chat", 90*time.Second, func(ctx context.Context) error {
		result, err := client.Complete(ctx, grok.ChatRequest{
			Model: "grok-chat-fast",
			Messages: []grok.Message{{
				Role:    "user",
				Content: "只用一个汉字回答：好",
			}},
		})
		if err != nil {
			return err
		}
		text := strings.TrimSpace(result.Text)
		if text == "" {
			return fmt.Errorf("空回复 conversation=%s", result.ConversationID)
		}
		fmt.Printf("text=%q conv=%s\n", truncate(text, 80), result.ConversationID)
		return nil
	})

	if *skipMedia {
		exit(failed)
	}

	if *onlyRefs {
		runVideoRefs(run, client, nil)
		exit(failed)
	}

	var imageURL string
	run("image-quality", 3*time.Minute, func(ctx context.Context) error {
		result, err := client.Complete(ctx, grok.ChatRequest{
			Model: "grok-imagine-image",
			Size:  "1:1",
			N:     1,
			Messages: []grok.Message{{
				Role:    "user",
				Content: "一只简笔画橘子，纯白背景，不要文字",
			}},
		})
		if err != nil {
			return err
		}
		if len(result.Images) == 0 {
			return fmt.Errorf("没有图片 URL text=%q", truncate(result.Text, 80))
		}
		if imageURL == "" {
			imageURL = result.Images[0]
		}
		fmt.Printf("images=%d url=%s\n", len(result.Images), truncate(imageURL, 96))
		return nil
	})

	if imageURL != "" {
		run("image-edit", 3*time.Minute, func(ctx context.Context) error {
			result, err := client.Complete(ctx, grok.ChatRequest{
				Model: "grok-imagine-image-edit",
				Size:  "1:1",
				Messages: []grok.Message{{
					Role: "user",
					Content: []any{
						map[string]any{"type": "text", "text": "改成一只简笔画苹果，纯白背景，不要文字"},
						map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}},
					},
				}},
			})
			if err != nil {
				return err
			}
			if len(result.Images) == 0 {
				return fmt.Errorf("没有编辑后的图片 text=%q", truncate(result.Text, 80))
			}
			fmt.Printf("edited=%s\n", truncate(result.Images[0], 96))
			return nil
		})
	}

	if *skipVideo {
		exit(failed)
	}

	run("image-lite", 3*time.Minute, func(ctx context.Context) error {
		result, err := client.Complete(ctx, grok.ChatRequest{
			Model: "grok-imagine-image-lite",
			Size:  "1:1",
			N:     1,
			Messages: []grok.Message{{
				Role:    "user",
				Content: "一只简笔画橘子，纯白背景，不要文字",
			}},
		})
		if err != nil {
			return err
		}
		if len(result.Images) == 0 {
			return fmt.Errorf("没有图片 URL text=%q", truncate(result.Text, 80))
		}
		imageURL = result.Images[0]
		fmt.Printf("images=%d url=%s\n", len(result.Images), truncate(imageURL, 96))
		return nil
	})

	run("video-text", 4*time.Minute, func(ctx context.Context) error {
		result, err := client.Complete(ctx, grok.ChatRequest{
			Model: "grok-imagine-video",
			Size:  "1:1",
			Messages: []grok.Message{{
				Role:    "user",
				Content: "一个红色小球在白背景上轻轻跳动，两秒即可",
			}},
		})
		if err != nil {
			return err
		}
		if result.VideoURL == "" {
			return fmt.Errorf("没有视频 URL text=%q", truncate(result.Text, 80))
		}
		fmt.Printf("video=%s\n", truncate(result.VideoURL, 96))
		return nil
	})

	var imageURLs []string
	if imageURL != "" {
		imageURLs = []string{imageURL}
	}
	runVideoRefs(run, client, imageURLs)

	if imageURL != "" {
		run("video-image", 4*time.Minute, func(ctx context.Context) error {
			result, err := client.Complete(ctx, grok.ChatRequest{
				Model: "grok-imagine-video",
				Size:  "1:1",
				Messages: []grok.Message{{
					Role: "user",
					Content: []any{
						map[string]any{"type": "text", "text": "让这张图轻微晃动"},
						map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}},
					},
				}},
			})
			if err != nil {
				return err
			}
			if result.VideoURL == "" {
				return fmt.Errorf("没有视频 URL text=%q", truncate(result.Text, 80))
			}
			fmt.Printf("video=%s\n", truncate(result.VideoURL, 96))
			return nil
		})
	}

	exit(failed)
}

func runVideoRefs(run func(string, time.Duration, func(context.Context) error), client *grok.Client, existing []string) {
	refs := append([]string(nil), existing...)
	prompts := []string{
		"一只简笔画橘子，纯白背景，不要文字",
		"一只简笔画蓝杯子，纯白背景，不要文字",
	}
	for i := 0; len(refs) < 2 && i < len(prompts); i++ {
		prompt := prompts[i]
		name := fmt.Sprintf("video-refs-image-%d", len(refs)+1)
		run(name, 3*time.Minute, func(ctx context.Context) error {
			result, err := client.Complete(ctx, grok.ChatRequest{
				Model: "grok-imagine-image",
				Size:  "1:1",
				N:     1,
				Messages: []grok.Message{{
					Role:    "user",
					Content: prompt,
				}},
			})
			if err != nil {
				return err
			}
			if len(result.Images) == 0 {
				return fmt.Errorf("没有图片 URL")
			}
			refs = append(refs, result.Images[0])
			fmt.Printf("ref=%s\n", truncate(result.Images[0], 96))
			return nil
		})
	}
	if len(refs) < 2 {
		fmt.Printf("\n跳过 video-refs：只有 %d 张参考图\n", len(refs))
		return
	}
	run("video-refs", 4*time.Minute, func(ctx context.Context) error {
		content := []any{
			map[string]any{"type": "text", "text": "两张参考图里的物体一起轻微晃动，两秒即可"},
		}
		for _, raw := range refs {
			content = append(content, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": raw},
			})
		}
		fmt.Printf("refs=%d branch=referenceToVideo\n", len(refs))
		result, err := client.Complete(ctx, grok.ChatRequest{
			Model:    "grok-imagine-video",
			Size:     "1:1",
			Messages: []grok.Message{{Role: "user", Content: content}},
		})
		if err != nil {
			return err
		}
		if result.VideoURL == "" {
			return fmt.Errorf("没有视频 URL text=%q", truncate(result.Text, 80))
		}
		fmt.Printf("video=%s\n", truncate(result.VideoURL, 96))
		return nil
	})
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func exit(failed int) {
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "\n%d 项失败\n", failed)
		os.Exit(1)
	}
	fmt.Println("\n全部通过")
	os.Exit(0)
}
