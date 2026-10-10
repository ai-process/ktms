package registry

import "github.com/ai-process/llm-proxy/internal/proxydb"

// Vendors the engine can construct adapters for. DeepSeek rides the OpenAI
// adapter with a different base URL.
const (
	VendorOpenAI   = "openai"
	VendorGoogle   = "google"
	VendorDeepSeek = "deepseek"
	VendorTypeSafe = "typesafe"
)

// DeepSeekDefaultEndpoint is used when a deepseek model has no endpoint override.
const DeepSeekDefaultEndpoint = "https://api.deepseek.com"

// TypeSafeDefaultEndpoint is used when a typesafe model has no endpoint override.
const TypeSafeDefaultEndpoint = "https://api.typesafe.ai"

// Efforts a model may serve and a request may ask for.
const (
	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"
)

// CapabilityGoogleSearch marks models that can serve enable_google_search.
const CapabilityGoogleSearch = "google_search"

// CapabilityNoStructuredOutput marks models whose API cannot be handed a JSON
// schema (DeepSeek rejects response_format json_schema outright). The shape is
// asked for in the prompt and checked by the router instead. Stated as the
// exception so a model that says nothing keeps the enforced behaviour.
const CapabilityNoStructuredOutput = "no_structured_output"

// CapabilityNoThinking marks models that reason by default unless told not to
// (DeepSeek V4 thinks at high effort, billed as output). The proxy turns it off.
const CapabilityNoThinking = "no_thinking"

// CapabilityTTS and CapabilityImage mark models that can serve the speech and
// image RPCs. Opt-in: a chain filtered for one of these keeps only models that
// declare it, so a text model can never be handed audio work.
const (
	CapabilityTTS   = "tts"
	CapabilityImage = "image"
)

// CapabilityJudge marks models that answer typed questions instead of writing
// text. Opt-in like the other modalities: a judge chain keeps only models that
// declare it, so a chat model can never be handed a judgment.
const CapabilityJudge = "judge"

// CapabilityBatch marks models that support batch processing.
const CapabilityBatch = "batch"

func ValidVendor(v string) bool {
	return v == VendorOpenAI || v == VendorGoogle || v == VendorDeepSeek || v == VendorTypeSafe
}

// SoftSchema reports whether this model needs the prompt-and-verify path.
func SoftSchema(m *proxydb.Model) bool {
	return HasCapability(m, CapabilityNoStructuredOutput)
}

func ValidEffort(e string) bool {
	return e == EffortLow || e == EffortMedium || e == EffortHigh
}
