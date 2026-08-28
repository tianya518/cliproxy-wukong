package sentinel

import (
	"encoding/json"
	"testing"
)

func TestConversationBodyMatchesChat202608(t *testing.T) {
	c := NewClient(Config{BearerToken: "test", Model: "gpt-5-6-thinking"})
	c.Logf = nil

	body := c.buildConversationBody(ChatOptions{Text: "只回复一个词：PING"})

	if body["action"] != "next" {
		t.Fatalf("action=%v", body["action"])
	}
	if body["client_prepare_state"] != "success" {
		t.Fatalf("client_prepare_state=%v，官网 Chat 在 prepare 成功后写 success", body["client_prepare_state"])
	}
	if _, ok := body["history_and_training_disabled"]; ok {
		t.Fatal("非临时对话不应带 history_and_training_disabled")
	}
	if body["thinking_effort"] != "extended" {
		t.Fatalf("thinking 模型 thinking_effort=%v", body["thinking_effort"])
	}

	mode, _ := body["conversation_mode"].(map[string]string)
	if mode["kind"] != "primary_assistant" {
		t.Fatalf("conversation_mode=%v", body["conversation_mode"])
	}

	names, _ := body["local_function_names"].([]string)
	if len(names) != 1 || names[0] != "local.continue_in_work" {
		t.Fatalf("local_function_names=%v", body["local_function_names"])
	}

	contracts, _ := body["model_response_contracts"].([]map[string]interface{})
	if len(contracts) != 1 || contracts[0]["id"] != "photo_upload_action.v1" {
		t.Fatalf("model_response_contracts=%v", body["model_response_contracts"])
	}

	ctx, _ := body["client_contextual_info"].(map[string]interface{})
	if ctx["app_name"] != "chatgpt.com" {
		t.Fatalf("app_name=%v", ctx["app_name"])
	}
	if ctx["has_web_push_capabilities"] != true {
		t.Fatalf("has_web_push_capabilities=%v", ctx["has_web_push_capabilities"])
	}
	if ctx["web_push_notification_permission"] != "default" {
		t.Fatalf("web_push_notification_permission=%v", ctx["web_push_notification_permission"])
	}

	msgs, _ := body["messages"].([]map[string]interface{})
	if len(msgs) != 1 {
		t.Fatalf("messages=%d", len(msgs))
	}
	meta, _ := msgs[0]["metadata"].(map[string]interface{})
	if _, ok := meta["developer_mode_connector_ids"]; ok {
		t.Fatal("Chat 标签官网不再带 developer_mode_connector_ids")
	}
	if _, ok := meta["selected_github_repos"]; ok {
		t.Fatal("Chat 标签官网不再带 selected_github_repos")
	}
	if _, ok := meta["selected_sources"]; !ok {
		t.Fatal("缺少 selected_sources")
	}

	// 结构能过 JSON，避免 interface 类型写错。
	if _, err := json.Marshal(body); err != nil {
		t.Fatal(err)
	}
}

func TestConversationBodyProjectGizmo(t *testing.T) {
	c := NewClient(Config{BearerToken: "test", Model: "gpt-5-6-thinking"})
	c.Logf = nil

	body := c.buildConversationBody(ChatOptions{
		Text:    "只回复一个词：PONG",
		GizmoID: "g-p-6a8e8a1819448191a8b160a75c670bf6",
	})
	mode, _ := body["conversation_mode"].(map[string]string)
	if mode["kind"] != "gizmo_interaction" {
		t.Fatalf("kind=%v", mode["kind"])
	}
	if mode["gizmo_id"] != "g-p-6a8e8a1819448191a8b160a75c670bf6" {
		t.Fatalf("gizmo_id=%v", mode["gizmo_id"])
	}
	if _, ok := body["gizmo_id"]; ok {
		t.Fatal("官网项目对话不在顶层带 gizmo_id")
	}
	if c.GizmoID() != "g-p-6a8e8a1819448191a8b160a75c670bf6" {
		t.Fatalf("client gizmo 未记住: %s", c.GizmoID())
	}

	// 续轮不传 GizmoID 也应沿用。
	body2 := c.buildConversationBody(ChatOptions{Text: "再来"})
	mode2, _ := body2["conversation_mode"].(map[string]string)
	if mode2["kind"] != "gizmo_interaction" || mode2["gizmo_id"] != c.GizmoID() {
		t.Fatalf("续轮 conversation_mode=%v", body2["conversation_mode"])
	}
}

func TestConversationBodyTempModeFlag(t *testing.T) {
	c := NewClient(Config{BearerToken: "test", TempMode: true, Model: "gpt-5-6-instant"})
	c.Logf = nil
	body := c.buildConversationBody(ChatOptions{Text: "hi"})
	if body["history_and_training_disabled"] != true {
		t.Fatalf("temp 模式应写 history_and_training_disabled=true，得到 %v", body["history_and_training_disabled"])
	}
	if _, ok := body["thinking_effort"]; ok {
		t.Fatalf("instant 模型不应带 thinking_effort: %v", body["thinking_effort"])
	}
}
