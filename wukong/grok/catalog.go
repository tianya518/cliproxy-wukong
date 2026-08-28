package grok

// 模型表来自 grok2api provider/web/catalog.go，对应 grok.com 网页产品，不是官方 xAI API。

type Capability string

const (
	CapabilityChat      Capability = "chat"
	CapabilityImage     Capability = "image"
	CapabilityImageEdit Capability = "image_edit"
	CapabilityVideo     Capability = "video"
)

type Tier string

const (
	TierAuto  Tier = "auto"
	TierBasic Tier = "basic"
	TierSuper Tier = "super"
	TierHeavy Tier = "heavy"
)

type ModelSpec struct {
	PublicID      string
	UpstreamModel string
	ProtocolModel string
	ImaginePro    bool
	Capability    Capability
	Mode          string
	MinimumTier   Tier
}

var catalog = []ModelSpec{
	{PublicID: "grok-chat-fast", UpstreamModel: "grok-chat-fast", Capability: CapabilityChat, Mode: "fast", MinimumTier: TierBasic},
	{PublicID: "grok-chat-auto", UpstreamModel: "grok-chat-auto", Capability: CapabilityChat, Mode: "auto", MinimumTier: TierSuper},
	{PublicID: "grok-chat-expert", UpstreamModel: "grok-chat-expert", Capability: CapabilityChat, Mode: "expert", MinimumTier: TierSuper},
	{PublicID: "grok-chat-heavy", UpstreamModel: "grok-chat-heavy", Capability: CapabilityChat, Mode: "heavy", MinimumTier: TierHeavy},
	{PublicID: "grok-imagine-image-lite", UpstreamModel: "grok-imagine-image", ProtocolModel: "imagine-lite", Capability: CapabilityImage, Mode: "fast", MinimumTier: TierBasic},
	{PublicID: "grok-imagine-image", UpstreamModel: "grok-imagine-image-quality", ProtocolModel: "imagine", Capability: CapabilityImage, Mode: "image_pro", MinimumTier: TierBasic},
	{PublicID: "grok-imagine-image-2.0", UpstreamModel: "grok-imagine-image-2.0", ProtocolModel: "imagine", ImaginePro: true, Capability: CapabilityImage, Mode: "image_pro", MinimumTier: TierBasic},
	{PublicID: "grok-imagine-image-edit", UpstreamModel: "imagine-image-edit", Capability: CapabilityImageEdit, Mode: "image_edit", MinimumTier: TierBasic},
	{PublicID: "grok-imagine-video", UpstreamModel: "grok-imagine-video", ProtocolModel: "imagine-video-gen", Capability: CapabilityVideo, Mode: "video", MinimumTier: TierBasic},
}

func Catalog() []ModelSpec {
	return append([]ModelSpec(nil), catalog...)
}

func PublicModelIDs() []string {
	out := make([]string, 0, len(catalog))
	for _, spec := range catalog {
		out = append(out, spec.PublicID)
	}
	return out
}

func Resolve(id string) (ModelSpec, bool) {
	for _, spec := range catalog {
		if spec.PublicID == id {
			return spec, true
		}
	}
	for _, spec := range catalog {
		if spec.UpstreamModel == id {
			return spec, true
		}
	}
	return ModelSpec{}, false
}

func TierSupports(actual, minimum Tier) bool {
	rank := map[Tier]int{TierBasic: 1, TierSuper: 2, TierHeavy: 3}
	if actual == TierAuto || actual == "" {
		actual = TierBasic
	}
	return rank[actual] >= rank[minimum]
}
