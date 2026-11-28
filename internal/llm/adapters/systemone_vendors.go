package adapters

import "time"

// Vendor profiles of System One, checked against each vendor's docs on
// 2026-11-28 (see ai-process/docs#297). Image limits are the documented ones;
// what a vendor does not document is left unchecked rather than guessed.

// LiquidProfile is Liquid AI's d1 family (docs.liquid.ai/lfm/models/d1).
var LiquidProfile = SystemOneProfile{
	Vendor:     "liquid",
	Path:       "/decisions/v1/systemone",
	AuthPrefix: "Bearer ",
	Media:      MediaImages,
	Images: &SystemOneImageLimits{
		Name: "Liquid", MinSide: 1, MinPixels: 1,
		MaxBody:   4_500_000, // images go as base64 inside it
		MaxImages: 8, MaxImagesWithBox: 1,
	},
}

// CloudflareProfile is Workers AI's Clef models: the endpoint is the account's
// /ai/run/@cf/cloudflare prefix, the model goes in the path and in the body.
var CloudflareProfile = SystemOneProfile{
	Vendor:     "cloudflare",
	Path:       "/{model}",
	AuthPrefix: "Bearer ",
	Media:      MediaImages,
	Images: &SystemOneImageLimits{
		Name: "Cloudflare", MinSide: 1, MinPixels: 1,
		MaxPixels: 16_000_000, MaxBytes: 4 << 20, MaxImages: 4,
		Formats: []string{"png", "jpg", "webp"},
	},
}

// PerplexityProfile is Perplexity's Decisions API (pplx-decider): System One's
// body on /v1/decisions, images as image_url parts inside the state.
var PerplexityProfile = SystemOneProfile{
	Vendor:     "perplexity",
	Path:       "/v1/decisions",
	AuthPrefix: "Bearer ",
	Media:      MediaStateParts,
	Images: &SystemOneImageLimits{
		Name: "Perplexity", MinSide: 1, MinPixels: 1,
		MaxPixels: 2048 * 32 * 32, // 2,048 tiles of 32x32
		MaxBytes:  16 << 20, MaxImages: 8,
	},
}

// MicrosoftProfile is Microsoft-Decision on a Foundry deployment, the model
// being the deployment name. Text only until Foundry documents how 1.1 takes
// images; the key goes in api-key like other Foundry endpoints.
var MicrosoftProfile = SystemOneProfile{
	Vendor:     "microsoft",
	Path:       "/providers/microsoft/v1/systemone",
	AuthHeader: "api-key",
}

// NodeSystemOneProfile is a decision model on a registered node behind its
// gateway: Ollama 0.35+, llama-server, SGLang and the vendors' own servers all
// take bare base64 images at the top level. The node's access key is the bearer.
var NodeSystemOneProfile = SystemOneProfile{
	Vendor:     "node",
	Path:       "/v1/systemone",
	AuthPrefix: "Bearer ",
	Media:      MediaImages,
	RawBase64:  true,
	Images: &SystemOneImageLimits{
		Name: "the node", MinSide: 1, MinPixels: 1,
		MaxBytes: 8 << 20,
		MaxBody:  32 << 20, // Ollama's request limit, images as base64 inside it
		Formats:  []string{"png", "jpg"}, StillOnly: true, UprightOnly: true,
	},
	MinTimeout:    30 * time.Second,
	GatewayErrors: true,
}
