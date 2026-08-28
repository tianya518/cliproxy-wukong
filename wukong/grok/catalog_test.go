package grok

import "testing"

func TestCatalogPublicIDs(t *testing.T) {
	ids := PublicModelIDs()
	want := []string{
		"grok-chat-fast", "grok-chat-auto", "grok-chat-expert", "grok-chat-heavy",
		"grok-imagine-image-lite", "grok-imagine-image", "grok-imagine-image-2.0",
		"grok-imagine-image-edit", "grok-imagine-video",
	}
	if len(ids) != len(want) {
		t.Fatalf("catalog size = %d, want %d: %v", len(ids), len(want), ids)
	}
	for i, id := range want {
		if ids[i] != id {
			t.Errorf("ids[%d] = %q, want %q", i, ids[i], id)
		}
	}
}

func TestResolveUsesPublicAndUpstream(t *testing.T) {
	spec, ok := Resolve("grok-imagine-image")
	if !ok || spec.ProtocolModel != "imagine" || spec.Capability != CapabilityImage {
		t.Fatalf("resolve public: %#v ok=%v", spec, ok)
	}
	spec, ok = Resolve("grok-imagine-image-quality")
	if !ok || spec.PublicID != "grok-imagine-image" {
		t.Fatalf("resolve upstream: %#v ok=%v", spec, ok)
	}
}

func TestTierSupports(t *testing.T) {
	if !TierSupports(TierBasic, TierBasic) || TierSupports(TierBasic, TierSuper) {
		t.Fatal("basic/super ranking")
	}
	if !TierSupports(TierAuto, TierBasic) || TierSupports("", TierHeavy) {
		t.Fatal("auto/empty treat as basic")
	}
}
