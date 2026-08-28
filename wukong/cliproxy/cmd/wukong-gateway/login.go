package main

// login.go —— OAuth 登录子命令。
//
// 登录只做一件事：把凭证写进 cfg.AuthDir。网关那边由 cliproxy 的 auth-dir
// watcher 负责发现——启动时全量扫一遍，运行中靠 fsnotify 增量加载——所以
// 登录完不需要重启网关，凭证会自己进池。
//
// 注意这里用的是 sdk/auth.Manager（负责登录与写盘），和 main.go 里的
// coreauth.Manager（负责运行时选凭证与执行）是两套东西，不要搞混。

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// codexDeviceFlow 是 CodexAuthenticator 约定的 metadata 开关，
// 设上之后走 device code 流程而不是本地回调。
const (
	codexLoginModeKey = "codex_login_mode"
	codexLoginModeDev = "device"
)

// stdinPrompt 在本地回调迟迟没来时，允许手工粘贴浏览器地址栏里的 callback URL。
// 远程开发机、WSL、端口被占这些情况下这是唯一的出路。
func stdinPrompt(prompt string) (string, error) {
	fmt.Print(prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func runLogin(ctx context.Context, cfg *sdkconfig.Config, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	noBrowser := fs.Bool("no-browser", false, "不自动打开浏览器，只打印授权链接")
	device := fs.Bool("device", false, "改用 device code 流程（无浏览器或无法本地回调时用）")
	port := fs.Int("port", 1455, "OAuth 本地回调端口")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: wukong-gateway login [provider] [选项]\n\n")
		fmt.Fprintf(os.Stderr, "provider 默认 codex，可选 claude / antigravity / kimi / xai。\n\n选项:\n")
		fs.PrintDefaults()
	}

	provider := "codex"
	// provider 作为位置参数，允许出现在选项前面。
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		provider = strings.ToLower(args[0])
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	store := sdkauth.GetTokenStore()
	// 不设 baseDir 的话凭证会落到默认的 ~/.cli-proxy-api，而网关看的是 cfg.AuthDir。
	if ds, ok := store.(interface{ SetBaseDir(string) }); ok {
		ds.SetBaseDir(cfg.AuthDir)
	}

	mgr := sdkauth.NewManager(store,
		sdkauth.NewCodexAuthenticator(),
		sdkauth.NewClaudeAuthenticator(),
		sdkauth.NewAntigravityAuthenticator(),
		sdkauth.NewKimiAuthenticator(),
		sdkauth.NewXAIAuthenticator(),
	)

	opts := &sdkauth.LoginOptions{
		NoBrowser:    *noBrowser,
		CallbackPort: *port,
		Prompt:       stdinPrompt,
	}
	if *device {
		opts.Metadata = map[string]string{codexLoginModeKey: codexLoginModeDev}
	}

	fmt.Printf("provider=%s 凭证目录=%s\n", provider, cfg.AuthDir)
	_, savedPath, err := mgr.Login(ctx, provider, cfg, opts)
	if err != nil {
		return fmt.Errorf("%s 登录失败: %w", provider, err)
	}
	if savedPath == "" {
		return fmt.Errorf("%s 登录成功但凭证没有落盘，检查 auth-dir 配置", provider)
	}

	fmt.Printf("\n登录成功，凭证已写入 %s\n", savedPath)
	fmt.Println("网关正在运行的话会自动加载，不需要重启。")
	return nil
}
