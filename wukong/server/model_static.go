package server

import "time"

// supportedModels 对外暴露的模型列表。
//
// 这只是**兜底**：服务启动后会用官网 /backend-api/models 的真实目录整体替换
// （见 model_catalog.go）。访问一律走 snapshotModels()，不要直接读这个变量——
// 刷新协程会替换它。
var supportedModels = []Model{
	{ID: "gpt-5-5-thinking", Object: "model", Created: 1700000000, OwnedBy: "openai"},
	{ID: "gpt-5-5", Object: "model", Created: 1700000000, OwnedBy: "openai"},
	{ID: "gpt-5-4-thinking", Object: "model", Created: 1700000000, OwnedBy: "openai"},
	{ID: "gpt-5-3-instant", Object: "model", Created: 1700000000, OwnedBy: "openai"},
	{ID: "o3", Object: "model", Created: 1700000000, OwnedBy: "openai"},
	// 图片生成（需配合 picture_v2；gpt-image-2 等别名会自动映射到此）
	{ID: "dall-e-3", Object: "model", Created: 1700000000, OwnedBy: "openai"},
}

func init() {
	ts := time.Now().Unix()
	for i := range supportedModels {
		supportedModels[i].Created = ts
	}
}
