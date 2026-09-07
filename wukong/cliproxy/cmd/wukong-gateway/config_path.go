package main

import (
	"os"
	"strings"
)

const defaultConfigPath = "config.yaml"

// parseGatewayArgs 抽出宝塔/官方同款的 --config，其余参数留给 login 子命令。
//
// 支持：
//
//	--config config.yaml
//	-config config.yaml
//	--config=config.yaml
//	单独一个 --config（没跟路径）→ 忽略，回落到环境变量或默认
func parseGatewayArgs(args []string, getenv func(string) string) (cfgPath string, rest []string) {
	cfgPath = defaultConfigPath
	if getenv != nil {
		if v := strings.TrimSpace(getenv("CLIPROXY_CONFIG")); v != "" {
			cfgPath = v
		}
	}
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			rest = append(rest, args[i+1:]...)
			return cfgPath, rest
		case arg == "-config" || arg == "--config":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				cfgPath = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "-config="):
			if v := strings.TrimPrefix(arg, "-config="); v != "" {
				cfgPath = v
			}
		case strings.HasPrefix(arg, "--config="):
			if v := strings.TrimPrefix(arg, "--config="); v != "" {
				cfgPath = v
			}
		default:
			rest = append(rest, arg)
		}
	}
	return cfgPath, rest
}

func resolveConfigPath() (cfgPath string, rest []string) {
	return parseGatewayArgs(os.Args[1:], os.Getenv)
}
