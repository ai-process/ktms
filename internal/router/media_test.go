package router

import (
	"context"
	"errors"
	"testing"

	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
)

// noThrottle admits everything: these tests are about chain walking, and the
// throttle paths are already covered for the text path.
type noThrottle struct {
	NoopThrottle
}

func mediaModels(ids ...string) []*proxydb.Model {
	var out []*proxydb.Model
	for _, id := range ids {
		out = append(out, &proxydb.Model{ID: id, Vendor: "google"})
	}
	return out
}

func mediaReq(chain []*proxydb.Model, call func(*proxydb.Model, llm.ClientAdapter) (llm.TokensUsage, error)) MediaRequest {
	return MediaRequest{
		Chain:   chain,
		KeyName: "generation",
		Call:    call,
		Adapter: func(*proxydb.Model) (llm.ClientAdapter, error) { return nil, nil },
	}
}

// A vendor that answers without audio must not end the run: the whole point of
// proxying speech is that another vendor gets a turn.
func TestExecuteMediaAdvancesOnRetryableFailure(t *testing.T) {
	var tried []string
	m, err := ExecuteMedia(context.Background(), noThrottle{}, mediaReq(
		mediaModels("gemini-tts", "openai-tts"),
		func(m *proxydb.Model, _ llm.ClientAdapter) (llm.TokensUsage, error) {
			tried = append(tried, m.ID)
			if m.ID == "gemini-tts" {
				return llm.TokensUsage{}, llm.ErrNoAudioContent
			}
			return llm.TokensUsage{Output: 7}, nil
		}))
	if err != nil {
		t.Fatalf("ExecuteMedia: %v", err)
	}
	if m.ID != "openai-tts" {
		t.Errorf("served by %q, want the second model", m.ID)
	}
	if len(tried) != 2 {
		t.Errorf("tried %v, want both models", tried)
	}
}

func TestExecuteMediaStopsOnTerminalError(t *testing.T) {
	terminal := errors.New("400 bad request")
	var tried []string
	_, err := ExecuteMedia(context.Background(), noThrottle{}, mediaReq(
		mediaModels("a", "b"),
		func(m *proxydb.Model, _ llm.ClientAdapter) (llm.TokensUsage, error) {
			tried = append(tried, m.ID)
			return llm.TokensUsage{}, terminal
		}))
	if !errors.Is(err, terminal) {
		t.Fatalf("err = %v, want the terminal error", err)
	}
	// A terminal error would fail the same way on the next model.
	if len(tried) != 1 {
		t.Errorf("tried %v, want only the first model", tried)
	}
}

func TestExecuteMediaExhaustsChain(t *testing.T) {
	_, err := ExecuteMedia(context.Background(), noThrottle{}, mediaReq(
		mediaModels("a", "b"),
		func(*proxydb.Model, llm.ClientAdapter) (llm.TokensUsage, error) {
			return llm.TokensUsage{}, llm.ErrNoAudioContent
		}))
	var exhausted *ChainExhausted
	if !errors.As(err, &exhausted) {
		t.Fatalf("err = %v, want ChainExhausted", err)
	}
}

// A model that declares tts but whose chain also holds text-only models must
// only ever be handed the modality it can serve — the capability filter is
// what guarantees that, so assert it directly.
func TestFilterChainRequiresCapability(t *testing.T) {
	models := []*proxydb.Model{
		model("flash", registry.VendorGoogle, registry.EffortLow),
		model("tts", registry.VendorGoogle, registry.EffortLow),
	}
	models[1].Capabilities = []string{registry.CapabilityTTS}
	keys := map[string]map[string]string{"key1": {registry.VendorGoogle: "g"}}
	snap := registry.NewSnapshotForTest(models, nil, keys, nil)

	chain, err := FilterChain(snap, []string{"flash", "tts"}, "key1", registry.EffortLow, false, registry.CapabilityTTS)
	if err != nil {
		t.Fatalf("FilterChain: %v", err)
	}
	if len(chain) != 1 || chain[0].ID != "tts" {
		t.Fatalf("chain = %+v, want only the tts model", chain)
	}
	// No capable model is a clearer failure than silently calling a text model.
	if _, err := FilterChain(snap, []string{"flash"}, "key1", registry.EffortLow, false, registry.CapabilityTTS); !errors.Is(err, ErrNoCapableModel) {
		t.Errorf("err = %v, want ErrNoCapableModel", err)
	}
	// Without a required capability the same chain is unfiltered.
	chain, err = FilterChain(snap, []string{"flash", "tts"}, "key1", registry.EffortLow, false, "")
	if err != nil || len(chain) != 2 {
		t.Errorf("unfiltered chain = %+v, err %v", chain, err)
	}
}

// A text rule must never capture media traffic. This is the trap the general
// matcher walks into: it ignores attributes the rule does not name, so a
// subject rule matches a speech request carrying that subject and hands back a
// chain with nothing capable.
func TestMatchModalityIgnoresTextRules(t *testing.T) {
	rules := []*proxydb.Rule{
		rule("zh-low", 99, registry.EffortLow, map[string]string{"subject": "zh"}, "flash"),
		rule("tts-default", 50, registry.EffortLow, map[string]string{"modality": "tts"}, "tts"),
		rule("low-default", 0, registry.EffortLow, nil, "flash"),
	}
	snap := registry.NewSnapshotForTest(nil, rules, nil, nil)
	attrs := map[string]string{"subject": "zh", "modality": "tts"}

	// The general matcher takes the higher-priority text rule.
	if got := Match(snap, registry.EffortLow, attrs); got == nil || got.Name != "zh-low" {
		t.Fatalf("Match picked %v; the test's premise no longer holds", got)
	}
	// The modality matcher skips it and finds the speech rule.
	got := MatchModality(snap, registry.EffortLow, "modality", "tts", attrs)
	if got == nil || got.Name != "tts-default" {
		t.Fatalf("MatchModality = %v, want tts-default", got)
	}
	// No image rule configured is nil, not the low catch-all.
	if got := MatchModality(snap, registry.EffortLow, "modality", "image", attrs); got != nil {
		t.Errorf("MatchModality for an unconfigured modality = %v, want nil", got)
	}
}
