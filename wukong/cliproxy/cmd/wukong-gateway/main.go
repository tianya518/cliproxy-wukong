// Command wukong-gateway 以 CLIProxyAPI 为网关运行，把 wukong 的 ChatGPT / Grok
// 网页逆向作为进程内原生 provider（chatgpt-web / grok-web）。
//
// 单一入口：只监听一个端口（config.yaml 的 host:port，默认 :8317）。
//   - OpenAI / Claude / Gemini 三套协议入口由 cliproxy 提供；
//   - 生图/沙箱产物代理（/api/image/proxy 等）、静态图片（/images）、账号管理
//     （/chatgpt|/tokens、/grok）由 wukong 的处理器直接挂在同一个 gin 引擎上
//     （见 server.RegisterArtifactAndAdminRoutes），走网关根路由、免 api-key。
//
// executor 与产物路由共用同一个 SessionManager，否则按 conversation_id 反查会话
// 会失败、图片取不到。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	sdkapi "github.com/router-for-me/CLIProxyAPI/v7/sdk/api"
	sdkapihandlers "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	sdkcliproxy "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"

	glue "github.com/router-for-me/CLIProxyAPI/v7/wukong/cliproxy"
	"github.com/router-for-me/CLIProxyAPI/v7/wukong/grok"
	sentinelserver "github.com/router-for-me/CLIProxyAPI/v7/wukong/server"
)

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func main() {
	cfgPath := env("CLIPROXY_CONFIG", "config.yaml")

	cfg, err := sdkconfig.LoadConfig(cfgPath)
	if err != nil {
		log.Fatalf("load cliproxy config %s: %v", cfgPath, err)
	}

	// login 子命令只做 OAuth 并写凭证文件，不起网关。
	if len(os.Args) > 1 && os.Args[1] == "login" {
		if err = runLogin(context.Background(), cfg, os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}

	// 生图/产物链接指向网关自身（单一入口）。默认用 config 的 host:port 推断；
	// 对外部署时用 ARTIFACT_BASE_URL 或 BASE_URL 覆盖成末端客户端可达的地址。
	gwHost := strings.TrimSpace(cfg.Host)
	if gwHost == "" || gwHost == "0.0.0.0" || gwHost == "::" {
		gwHost = "127.0.0.1"
	}
	gatewayBase := env("ARTIFACT_BASE_URL", env("BASE_URL", fmt.Sprintf("http://%s:%d", gwHost, cfg.Port)))

	// wukong 侧配置照常从环境变量读，复用现有那套开关。
	sentinelCfg := sentinelserver.LoadConfig()
	sentinelCfg.BaseURL = gatewayBase

	pool := sentinelserver.NewTokenPool(sentinelCfg.ChatGPTFile,
		time.Duration(sentinelCfg.TokenRefreshAheadSec)*time.Second)
	pool.SetOAuthConfig(sentinelCfg.OAuthTokenURL, sentinelCfg.OAuthClientID)
	if sentinelCfg.RefreshLoopSec > 0 {
		pool.StartRefreshLoop(time.Duration(sentinelCfg.RefreshLoopSec) * time.Second)
	}

	// 共用一个 SessionManager：executor 与产物路由必须看到同一批会话。
	session := sentinelserver.NewSessionManager(&sentinelCfg)
	engine := sentinelserver.NewEngine(&sentinelCfg, nil, session)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 官网模型目录同步，驱动 model 解析与对外模型列表。
	sentinelserver.StartModelCatalogSync(&sentinelCfg, pool)

	core := coreauth.NewManager(nil, nil, nil)
	exec := glue.NewExecutor(engine, gatewayBase)
	core.RegisterExecutor(exec)
	grokCfg := grok.ConfigFromEnv()
	if sentinelCfg.ProxyURL != "" {
		grokCfg.ProxyURL = sentinelCfg.ProxyURL
	}
	grokStore := grok.NewAccountStore(sentinelCfg.GrokFile, grokCfg)
	grokExec := glue.NewGrokExecutor(grokStore.ClientConfig())
	core.RegisterExecutor(grokExec)

	// 把两条网页逆向注册为 cliproxy 的进程内原生 provider：executor 绑定与模型注册
	// 从此走内置 provider 同款重载路径，config/auths 热重载中天然存活（见 cliproxy/models.go）。
	glue.RegisterNativeProviders(exec, grokExec)

	// svc 在 Build 之后才有；grok 运行时增删账号后要用它触发一次模型重注册。
	var svc *sdkcliproxy.Service

	registered, err := glue.RegisterAuthsFromChatGPTFile(ctx, core, sentinelCfg.ChatGPTFile)
	if err != nil {
		log.Fatalf("register chatgpt credentials: %v", err)
	}
	log.Printf("[startup] 已注册 %d 个 ChatGPT 凭证到 cliproxy 凭证池", registered)
	if registered == 0 {
		log.Printf("[startup] 警告：%s 里没有可用凭证，chatgpt-web 请求会因无凭证被拒", sentinelCfg.ChatGPTFile)
	}
	grokStore.SetOnChange(func(accounts []grok.Credential) {
		if err := glue.ReplaceGrokAuths(context.Background(), core, accounts); err != nil {
			log.Printf("[grok] 同步 cliproxy 凭证失败: %v", err)
			return
		}
		// 新增账号的模型不会自动注册，主动补一次。
		if svc != nil {
			svc.RefreshNativeProviderModels(glue.GrokProviderKey)
		}
	})
	grokRegistered, err := glue.RegisterGrokAuths(ctx, core, grokStore.Snapshot())
	if err != nil {
		log.Fatalf("register grok credentials: %v", err)
	}
	if mode := grokStore.ClientConfig().ClearanceMode; mode != grok.ClearanceModeManual {
		log.Printf("[startup] Grok Clearance mode=%s solver=%s", mode, grokStore.ClientConfig().FlareSolverrURL)
	}
	log.Printf("[startup] 已注册 %d 个 Grok Web 凭证到 cliproxy 凭证池", grokRegistered)
	if grokRegistered == 0 {
		log.Printf("[startup] 提示：%s 里没有 Grok Web 账号，可用 POST /grok/upload 灌入", sentinelCfg.GrokFile)
	}

	svc, err = sdkcliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(cfgPath).
		WithCoreAuthManager(core).
		WithServerOptions(
			// 单一入口：把 wukong 的产物代理与账号管理路由挂到网关的 gin 引擎上。
			// 这些路由注册在根引擎、不进 cliproxy 的 api-key 鉴权组（图片链接要能被
			// 末端客户端直接取）。cliproxy 无 /api/*、/images、/chatgpt、/grok，不冲突。
			sdkapi.WithRouterConfigurator(func(ginEngine *gin.Engine, _ *sdkapihandlers.BaseAPIHandler, _ *sdkconfig.Config) {
				sentinelserver.RegisterArtifactAndAdminRoutes(ginEngine, &sentinelCfg, pool, session, grokStore)
			}),
		).
		WithHooks(sdkcliproxy.Hooks{
			OnAfterStart: func(started *sdkcliproxy.Service) {
				// service 启动不会主动为“Build 前程序化注册”的 auth 注册模型，
				// 这里触发一次初始注册；之后 config/auths 热重载由 fork 补丁的
				// 原生 provider 钩子兜住，不再需要旧版的抢回轮询。
				started.RefreshNativeProviderModels(glue.ProviderKey)
				started.RefreshNativeProviderModels(glue.GrokProviderKey)
			},
		}).
		Build()
	if err != nil {
		log.Fatalf("build cliproxy service: %v", err)
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		log.Printf("[shutdown] 收到退出信号")
		cancel()
	}()

	log.Printf("[startup] 单一入口 http://%s:%d providers=%s,%s 产物基址=%s",
		gwHost, cfg.Port, glue.ProviderKey, glue.GrokProviderKey, gatewayBase)
	if err = svc.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("cliproxy service: %v", err)
	}
}
