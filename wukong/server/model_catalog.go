package server

// model_catalog.go —— 启动时及定期从官网拉取模型目录，驱动 model 解析与 /v1/models。
//
// 拉取失败不影响服务可用：sentinel 侧会退回静态表，/v1/models 保持内置列表。

import (
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	sentinel "github.com/router-for-me/CLIProxyAPI/v7/wukong/sentinel"
)

// errNoTokenForCatalog 池内暂无可用凭证，拉取推迟到下一个周期。
var errNoTokenForCatalog = errors.New("no chatgpt-web access token")

// catalogRefreshInterval 目录刷新间隔。官网模型阵容以周/月为单位变动，无需频繁拉取。
const catalogRefreshInterval = 6 * time.Hour

// modelsMu 保护 supportedModels——启动后会被刷新协程替换。
var modelsMu sync.RWMutex

// StartModelCatalogSync 启动时拉取一次模型目录，随后定期刷新。
// pickToken 为空或暂无可用 AT 时静默跳过，服务继续用静态表。
func StartModelCatalogSync(cfg *ServerConfig, pickToken func() (string, bool)) {
	if pickToken == nil {
		return
	}
	refresh := func() {
		if err := syncModelCatalog(cfg, pickToken); err != nil {
			log.Printf("[models] 目录同步失败（继续使用静态表）: %v", err)
		}
	}
	go func() {
		refresh()
		ticker := time.NewTicker(catalogRefreshInterval)
		defer ticker.Stop()
		for range ticker.C {
			refresh()
		}
	}()
}

// syncModelCatalog 用任一可用 Access Token 拉取目录并安装。
func syncModelCatalog(cfg *ServerConfig, pickToken func() (string, bool)) error {
	if pickToken == nil {
		return errNoTokenForCatalog
	}
	token, ok := pickToken()
	if !ok {
		return errNoTokenForCatalog
	}
	client := sentinel.NewClient(sentinel.Config{
		BearerToken: token,
		ProxyURL:    cfg.ProxyURL,
	})
	cat, err := client.FetchModelCatalog()
	if err != nil {
		return err
	}

	sentinel.SetModelCatalog(cat)
	setSupportedModels(buildModelList(cat))
	log.Printf("[models] 已同步官网模型目录：%d 个 slug（%s）",
		len(cat.Models), strings.Join(firstN(cat.Slugs(), 6), ", "))
	return nil
}

// buildModelList 把目录转成 /v1/models 的返回列表。
// 可配强度的 slug 额外暴露 -standard / -extended 两个显式变体，
// 否则标准 OpenAI 客户端只能拿到默认档，没法选官网的「高」。
func buildModelList(cat *sentinel.ModelCatalog) []Model {
	ts := time.Now().Unix()
	out := make([]Model, 0, len(cat.Models)+8)
	seen := make(map[string]bool)
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, Model{ID: id, Object: "model", Created: ts, OwnedBy: "openai"})
	}

	for _, m := range cat.Models {
		if m.HideFromChatCatalog() {
			continue
		}
		add(m.Slug)
		if m.ConfigurableThinkingEffort {
			for _, e := range m.ThinkingEfforts {
				add(m.Slug + "-" + e)
			}
		}
	}
	// 生图不是目录里的 model，而是所有模型都带的 image_gen 工具，需单独暴露
	add(sentinel.ModelDALLE3)
	return out
}

func setSupportedModels(list []Model) {
	if len(list) == 0 {
		return
	}
	modelsMu.Lock()
	supportedModels = list
	modelsMu.Unlock()
}

func snapshotModels() []Model {
	modelsMu.RLock()
	defer modelsMu.RUnlock()
	return supportedModels
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(s[:n:n], "...")
}
