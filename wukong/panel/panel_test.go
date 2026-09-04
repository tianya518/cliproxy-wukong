package panel

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestEmbeddedPanelCarriesGrokWebQuota(t *testing.T) {
	for _, marker := range []string{"grok_web_quota", "/grok/quota", "grokWebQuota"} {
		if !bytes.Contains(managementHTML, []byte(marker)) {
			t.Fatalf("embedded management.html lacks %q; rebuild the CPAMC fork and copy dist/index.html", marker)
		}
	}
}

func TestInstallWritesOnceAndPinsAutoUpdate(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MANAGEMENT_STATIC_PATH", "")
	t.Setenv("WRITABLE_PATH", "")

	cfg := &config.Config{}
	first, err := Install(cfgPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Written || !first.ConfigMissingPin || !cfg.RemoteManagement.DisableAutoUpdatePanel {
		t.Fatalf("first install = %+v pinned=%v", first, cfg.RemoteManagement.DisableAutoUpdatePanel)
	}
	want := filepath.Join(dir, "static", "management.html")
	if first.Path != want {
		t.Fatalf("path = %q want %q", first.Path, want)
	}
	data, err := os.ReadFile(want)
	if err != nil || !bytes.Equal(data, managementHTML) {
		t.Fatalf("written panel mismatch err=%v len=%d", err, len(data))
	}

	// 已经是同一份内容：不重写；config 已钉住时不再报缺失。
	second, err := Install(cfgPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if second.Written || second.ConfigMissingPin {
		t.Fatalf("second install = %+v", second)
	}

	// 上游更新器把文件换掉后，下次启动要换回来。
	if err = os.WriteFile(want, []byte("<html>upstream</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := Install(cfgPath, cfg)
	if err != nil || !third.Written {
		t.Fatalf("third install = %+v err=%v", third, err)
	}
}

func TestInstallSkipsWhenControlPanelDisabled(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	t.Setenv("MANAGEMENT_STATIC_PATH", "")
	t.Setenv("WRITABLE_PATH", "")
	cfg := &config.Config{}
	cfg.RemoteManagement.DisableControlPanel = true
	result, err := Install(cfgPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.Written || result.Path != "" {
		t.Fatalf("result = %+v", result)
	}
	if _, err = os.Stat(filepath.Join(dir, "static", "management.html")); !os.IsNotExist(err) {
		t.Fatalf("panel should not be written when the control panel is disabled: %v", err)
	}
}
