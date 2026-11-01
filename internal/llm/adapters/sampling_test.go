package adapters

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ai-process/ktms/internal/llm"
)

func samplingChat() *llm.ChatContext {
	t, p, seed := 0.3, 0.8, int64(42)
	chat := llm.NewChatContext()
	chat.AddUserMessage("go")
	chat.SetSampling(llm.Sampling{Temperature: &t, TopP: &p, Stop: []string{"END", "STOP"}, Seed: &seed})
	return chat
}

// wire is the request as the vendor reads it.
func wire(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestOpenAISendsSampling(t *testing.T) {
	a := &OpenAIAdapter{sampling: llm.AllSampling}
	got := wire(t, a.buildCompletionParams(samplingChat(), nil, 0))
	if got["temperature"] != 0.3 || got["top_p"] != 0.8 || got["seed"] != 42.0 {
		t.Fatalf("sampling not sent: %v", got)
	}
	if stop, _ := json.Marshal(got["stop"]); string(stop) != `["END","STOP"]` {
		t.Fatalf("stop: %s", stop)
	}

	// A reasoning model's request carries none of them.
	got = wire(t, (&OpenAIAdapter{}).buildCompletionParams(samplingChat(), nil, 0))
	for _, k := range []string{"temperature", "top_p", "stop", "seed"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s sent to a model that does not take it", k)
		}
	}
}

func TestAnthropicSendsSampling(t *testing.T) {
	a := &AnthropicAdapter{model: "claude-sonnet-4-6", sampling: llm.SamplingSupport{Temperature: true, TopP: true, Stop: true, MaxTemperature: 1, OneOfTemperatureTopP: true}}
	got := wire(t, a.buildParams(samplingChat(), 0))
	if got["temperature"] != 0.3 || got["top_p"] != nil {
		t.Fatalf("want temperature alone: %v", got)
	}
	if stop, _ := json.Marshal(got["stop_sequences"]); string(stop) != `["END","STOP"]` {
		t.Fatalf("stop_sequences: %s", stop)
	}
	got = wire(t, (&AnthropicAdapter{model: "claude-opus-5-5", sampling: llm.SamplingSupport{Stop: true}}).buildParams(samplingChat(), 0))
	if _, ok := got["temperature"]; ok {
		t.Fatalf("temperature sent to a model that rejects it: %v", got)
	}
}

func TestGoogleSendsSampling(t *testing.T) {
	c := (&GoogleAdapter{sampling: llm.AllSampling}).buildGenerateConfig(samplingChat())
	if c.Temperature == nil || *c.Temperature != 0.3 || c.TopP == nil || *c.TopP != 0.8 ||
		c.Seed == nil || *c.Seed != 42 || !slices.Equal(c.StopSequences, []string{"END", "STOP"}) {
		t.Fatalf("sampling not set: %+v", c)
	}
	if c := (&GoogleAdapter{}).buildGenerateConfig(samplingChat()); c.Temperature != nil || c.StopSequences != nil {
		t.Fatal("sampling set without support")
	}
}

func TestBedrockSendsSampling(t *testing.T) {
	a := &BedrockAdapter{model: "amazon.nova-pro-v1:0", sampling: llm.SamplingSupport{Temperature: true, TopP: true, Stop: true, MaxTemperature: 1, MaxStop: 4}}
	in := a.buildConverseInput(samplingChat(), 0)
	inf := in.InferenceConfig
	if inf == nil || inf.MaxTokens != nil || *inf.Temperature != 0.3 || *inf.TopP != 0.8 || strings.Join(inf.StopSequences, ",") != "END,STOP" {
		t.Fatalf("inference config: %+v", inf)
	}
	if in := (&BedrockAdapter{}).buildConverseInput(samplingChat(), 0); in.InferenceConfig != nil {
		t.Fatalf("inference config without ceiling or sampling: %+v", in.InferenceConfig)
	}
}
