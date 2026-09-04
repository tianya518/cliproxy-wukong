// Package panel 内嵌 wukong 定制版的 CPAMC 管理面板（management.html）。
//
// 上游 CLIProxyAPI 会从 GitHub Release 拉最新面板并覆盖本地文件，而上游面板不认识
// chatgpt-web / grok-web 这两个自有 provider，额度区块渲染不出来。这里把 fork 构建出的
// 单文件面板随二进制一起发，启动时写进 SDK 实际服务的 static 目录，并钉住
// disable-auto-update-panel，让上游的更新器跳过。面板路由、安全模式提示等仍由上游处理。
//
// 面板源码：https://github.com/router-for-me/Cli-Proxy-API-Management-Center 的 fork，
// 新增 src/features/quota/providers/grok-web。重新构建后把 dist/index.html 复制到本目录
// 覆盖 management.html 即可。
package panel

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/managementasset"
)

//go:embed management.html
var managementHTML []byte

// InstallResult 描述一次 Install 的结果，供启动日志用。
type InstallResult struct {
	// Path 是面板最终落盘位置（SDK 从这里服务 /management.html）。
	Path string
	// Written 为 true 表示本次覆盖了磁盘文件（首次安装或内容有差异）。
	Written bool
	// Hash 是内嵌面板的 sha256，便于和上游 release 的 digest 对照。
	Hash string
	// ConfigMissingPin 为 true 表示 config 文件里没写 disable-auto-update-panel: true，
	// 本次只在内存里钉住；热重载后可能被上游更新器覆盖。
	ConfigMissingPin bool
}

// Install 把内嵌面板写到 SDK 的 static 目录，并把 cfg.RemoteManagement.DisableAutoUpdatePanel
// 置为 true。cfgPath 是 config.yaml 路径（决定 static 目录，可被 MANAGEMENT_STATIC_PATH 覆盖）。
func Install(cfgPath string, cfg *config.Config) (InstallResult, error) {
	sum := sha256.Sum256(managementHTML)
	result := InstallResult{Hash: hex.EncodeToString(sum[:])}
	if cfg != nil {
		result.ConfigMissingPin = !cfg.RemoteManagement.DisableAutoUpdatePanel
		cfg.RemoteManagement.DisableAutoUpdatePanel = true
	}
	if cfg != nil && cfg.RemoteManagement.DisableControlPanel {
		return result, nil
	}

	target := strings.TrimSpace(managementasset.FilePath(cfgPath))
	if target == "" {
		return result, fmt.Errorf("management panel: cannot resolve static path from %q", cfgPath)
	}
	result.Path = target

	if existing, err := os.ReadFile(target); err == nil && bytes.Equal(existing, managementHTML) {
		return result, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return result, fmt.Errorf("management panel: prepare %s: %w", filepath.Dir(target), err)
	}
	if err := atomicWrite(target, managementHTML); err != nil {
		return result, fmt.Errorf("management panel: write %s: %w", target, err)
	}
	result.Written = true
	return result, nil
}

// Bytes 返回内嵌面板内容（测试与诊断用）。
func Bytes() []byte {
	return managementHTML
}

func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "management-*.html")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Chmod(0o644); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
