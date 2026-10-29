package registry

import (
	"testing"

	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/proxydb"
)

func TestSamplingSupport(t *testing.T) {
	claudeOld := llm.SamplingSupport{Temperature: true, TopP: true, Stop: true, MaxTemperature: 1, OneOfTemperatureTopP: true}
	claudeNew := llm.SamplingSupport{Stop: true}
	openAI := llm.AllSampling
	openAI.MaxStop = 4
	cases := []struct {
		vendor, id string
		caps       []string
		want       llm.SamplingSupport
	}{
		{VendorOpenAI, "gpt-4.1-mini", nil, openAI},
		{VendorOpenAI, "gpt-5-mini", nil, llm.SamplingSupport{}},
		{VendorOpenAI, "o4-mini", nil, llm.SamplingSupport{}},
		{VendorOpenAI, "gpt-5-chat-latest", nil, openAI},
		{VendorOpenAI, "gpt-4.1-mini", []string{CapabilityNoSampling}, llm.SamplingSupport{}},
		{VendorAnthropic, "claude-sonnet-4-6", nil, claudeOld},
		{VendorAnthropic, "claude-haiku-4-5-20251001", nil, claudeOld},
		{VendorAnthropic, "claude-sonnet-4-20250514", nil, claudeOld},
		{VendorAnthropic, "claude-3-5-haiku-latest", nil, claudeOld},
		{VendorAnthropic, "claude-opus-4-7", nil, claudeNew},
		{VendorAnthropic, "claude-opus-5-5", nil, claudeNew},
		{VendorAnthropic, "claude-sonnet-4-60", nil, claudeNew},
		{VendorBedrock, "us.anthropic.claude-sonnet-4-5-20250929-v1:0", nil, claudeOld},
		{VendorBedrock, "anthropic.claude-opus-4-8", nil, claudeNew},
		{VendorBedrock, "amazon.nova-pro-v1:0", nil, llm.SamplingSupport{Temperature: true, TopP: true, Stop: true, MaxTemperature: 1, MaxStop: 4}},
		{VendorMistral, "mistral-small-latest", nil, llm.SamplingSupport{Temperature: true, TopP: true, Stop: true, MaxTemperature: 1.5}},
		{VendorDeepSeek, "deepseek-chat", nil, llm.SamplingSupport{Temperature: true, TopP: true, Stop: true, MaxStop: 16}},
		{VendorLocal, "qwen3:8b", nil, llm.AllSampling},
		{VendorTypeSafe, "typesafe", nil, llm.SamplingSupport{}},
	}
	for _, c := range cases {
		m := &proxydb.Model{ID: c.id, Vendor: c.vendor, Capabilities: c.caps}
		if got := SamplingSupport(m); got != c.want {
			t.Errorf("%s %s: got %+v, want %+v", c.vendor, c.id, got, c.want)
		}
	}
}
