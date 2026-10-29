package registry

import (
	"regexp"
	"strings"

	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/proxydb"
)

// CapabilityNoSampling marks a model whose API rejects sampling fields the
// table below would send it: the proxy then sends none of them.
const CapabilityNoSampling = "no_sampling"

var (
	// OpenAI reasoning models reject temperature, top_p and stop;
	// gpt-5-chat is the chat variant that takes them.
	openAIReasoning = regexp.MustCompile(`^(o\d|gpt-5)`)
	// Claude up to 4.6 takes temperature and top_p on the 2023-06-01 API; 4.7
	// and later accept only the defaults, so unknown ids get none.
	claudeWithSampling = regexp.MustCompile(`claude-(3([-.@:]|$)|(opus|sonnet|haiku)-4(-[0-6])?(-\d{8}|-v\d|[.@:]|$))`)
)

// SamplingSupport is the sampling a model's API takes. Adapters send only
// that, and responses name the rest in ignored_parameters.
func SamplingSupport(m *proxydb.Model) llm.SamplingSupport {
	if HasCapability(m, CapabilityNoSampling) {
		return llm.SamplingSupport{}
	}
	id := strings.ToLower(m.ID)
	switch m.Vendor {
	case VendorOpenAI, VendorAzure:
		if openAIReasoning.MatchString(id) && !strings.Contains(id, "-chat") {
			return llm.SamplingSupport{}
		}
		s := llm.AllSampling
		s.MaxStop = 4
		return s
	case VendorGroq:
		s := llm.AllSampling
		s.MaxStop = 4
		return s
	case VendorDeepSeek:
		return llm.SamplingSupport{Temperature: true, TopP: true, Stop: true, MaxStop: 16}
	case VendorMistral:
		// Mistral names its seed random_seed and rejects unknown fields.
		return llm.SamplingSupport{Temperature: true, TopP: true, Stop: true, MaxTemperature: 1.5}
	case VendorOllama, VendorLocal:
		return llm.AllSampling
	case VendorGoogle:
		s := llm.AllSampling
		s.MaxStop = 5
		return s
	case VendorAnthropic:
		return claudeSampling(id)
	case VendorBedrock:
		if strings.Contains(id, "claude") {
			return claudeSampling(id)
		}
		// Converse takes temperature 0..1 and no seed for every model family.
		return llm.SamplingSupport{Temperature: true, TopP: true, Stop: true, MaxTemperature: 1, MaxStop: 4}
	}
	return llm.SamplingSupport{}
}

func claudeSampling(id string) llm.SamplingSupport {
	s := llm.SamplingSupport{Stop: true}
	if claudeWithSampling.MatchString(id) {
		// Claude reads temperature 0..1, and its 4.x models reject both
		// fields together.
		s.Temperature, s.TopP, s.MaxTemperature, s.OneOfTemperatureTopP = true, true, 1, true
	}
	return s
}
