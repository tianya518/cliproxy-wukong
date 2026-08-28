package grok

import "testing"

func TestTurnStateDoesNotReuseLastConversation(t *testing.T) {
	client := NewClient(Config{}, Credential{SSOToken: "test-sso"})
	client.SetConversation("old-conv", "old-parent")

	if got := client.turnState(ChatRequest{}); got != nil {
		t.Fatalf("empty conversation_id should start a new session, got %#v", got)
	}

	same := client.turnState(ChatRequest{ConversationID: "old-conv"})
	if same == nil || same.ConversationID != "old-conv" || same.ParentID != "old-parent" {
		t.Fatalf("explicit same conversation should keep parent, got %#v", same)
	}

	other := client.turnState(ChatRequest{ConversationID: "other-conv"})
	if other == nil || other.ConversationID != "other-conv" || other.ParentID != "" {
		t.Fatalf("other conversation should not inherit parent, got %#v", other)
	}

	explicit := client.turnState(ChatRequest{ConversationID: "other-conv", ParentID: "parent-2"})
	if explicit == nil || explicit.ParentID != "parent-2" {
		t.Fatalf("explicit parent_id should win, got %#v", explicit)
	}
}
