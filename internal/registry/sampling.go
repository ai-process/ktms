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

// CapabilitySampling marks a model that takes its vendor's sampling fields
// although its id looks like one that does not: an Azure deployment name, a
// Claude alias, a reasoning model run without reasoning.
const CapabilitySampling = "sampling"

var (
	// OpenAI reasoning models reject temperature, top_p and stop; whether they
	// take seed is unconfirmed, so it is dropped too. gpt-5-chat takes all four.
	openAIReasoning = regexp.MustCompile(`(^|[/_-])(o\d|gpt-5|codex-mini|computer-use)`)
	// Claude up to 4.6 takes temperature and top_p on the 2023-06-01 API; 4.7
	// and later accept only the defaults, so unknown ids get none.
	claudeWithSampling = regexp.MustCompile(`claude-(3([-.@:]|$)|(opus|sonnet|haiku)-4([-.][0-6])?(-\d{8}|-v\d|-latest|[@:]|$))`)
	claude3            = regexp.MustCompile(`claude-3([-.@:]|$)`)
)

// SamplingSupport is the sampling a model's API takes. Adapters send only
// that, and responses name the rest in ignored_parameters.
func SamplingSupport(m *proxydb.Model) llm.SamplingSupport {
	if HasCapability(m, CapabilityNoSampling) {
		return llm.SamplingSupport{}
	}
	declared := HasCapability(m, CapabilitySampling)
	id := strings.ToLower(m.ID)
	switch m.Vendor {
	case VendorOpenAI, VendorAzure:
		if !declared && openAIReasoning.MatchString(id) && !strings.Contains(id, "-chat") {
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
		return claudeSampling(id, declared)
	case VendorBedrock:
		if strings.Contains(id, "claude") {
			return claudeSampling(id, declared)
		}
		// An application inference profile hides its model, Claude perhaps:
		// stop alone, or with sampling declared what every family takes.
		if strings.Contains(id, ":application-inference-profile/") {
			s := llm.SamplingSupport{Stop: true, NoBlankStop: true, MaxStop: 4}
			if declared {
				s.Temperature, s.TopP, s.MaxTemperature, s.OneOfTemperatureTopP = true, true, 1, true
			}
			return s
		}
		// Converse takes temperature 0..1 and no seed for every model family.
		return llm.SamplingSupport{Temperature: true, TopP: true, Stop: true, MaxTemperature: 1, MaxStop: 4}
	}
	return llm.SamplingSupport{}
}

// claudeSampling: Claude rejects a stop sequence of whitespace alone, reads
// temperature 0..1, and from 4 on rejects temperature and top_p together.
func claudeSampling(id string, declared bool) llm.SamplingSupport {
	s := llm.SamplingSupport{Stop: true, NoBlankStop: true}
	if declared || claudeWithSampling.MatchString(id) {
		s.Temperature, s.TopP, s.MaxTemperature = true, true, 1
		s.OneOfTemperatureTopP = !claude3.MatchString(id)
	}
	return s
}
