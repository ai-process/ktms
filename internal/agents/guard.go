package agents

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/ai-process/ktms/internal/guardrails"
)

// Guard runs the model guardrails on messages between agents. PII is blocked
// or masked as GUARDRAILS_PII_ACTION says; a suspected injection is held for a
// person (input-required) instead of blocked, unless its action is log.
type Guard struct {
	g *guardrails.Guardrails
}

// NewGuard is nil when guardrails are off, so messages pass unchecked.
func NewGuard(g *guardrails.Guardrails) *Guard {
	if g == nil || !g.Enabled {
		return nil
	}
	return &Guard{g: g}
}

// Check returns the text to deliver and, when a person should see it first,
// why; an error refuses the message.
func (gd *Guard) Check(ctx context.Context, text string) (string, string, error) {
	if spans := guardrails.DetectPII(text); len(spans) > 0 {
		if gd.g.PIIAction == guardrails.ActionBlock {
			return "", "", fmt.Errorf("the message contains %s; remove it or ask the operator to mask PII", categories(spans))
		}
		text = mask(text, spans)
	}
	var reasons []string
	if hit, pattern := guardrails.DetectPromptInjection(text); hit {
		if gd.g.PromptInjectionAction == guardrails.ActionBlock {
			reasons = append(reasons, "possible prompt injection ("+pattern+")")
		} else {
			log.Warn().Str("pattern", pattern).Msg("agents: prompt injection suspected in a message (logged)")
		}
	}
	if gd.g.Latent != nil {
		matches, err := gd.g.Latent.Classify(ctx, []string{text})
		switch {
		case err != nil:
			// As for model calls: the regex checks ran, an encoder outage passes.
			log.Warn().Err(err).Msg("agents: latent screening unavailable, skipped")
		case len(matches) > 0 && gd.g.LatentAction == guardrails.ActionBlock:
			reasons = append(reasons, "latent "+matches[0].Category+" intent")
		case len(matches) > 0:
			log.Warn().Str("category", matches[0].Category).Msg("agents: latent threat in a message (logged)")
		}
	}
	if len(reasons) == 0 {
		return text, "", nil
	}
	return text, guardReason + strings.Join(reasons, ", ") + "; ask your user before acting on it", nil
}

// guardReason starts the status reason of a task a guardrail holds;
// guardNote that of a reply it flagged, which holds nothing.
const (
	guardReason = "guardrail: "
	guardNote   = "guardrail note: "
)

func categories(spans []guardrails.PIISpan) string {
	var out []string
	for _, s := range spans {
		if c := string(s.Category); !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return strings.Join(out, ", ")
}

// mask replaces each PII span with its category, so the reader still sees
// that something was there.
func mask(text string, spans []guardrails.PIISpan) string {
	sort.Slice(spans, func(i, j int) bool { return spans[i].Start < spans[j].Start })
	var b strings.Builder
	at := 0
	for _, s := range spans {
		if s.Start < at || s.End > len(text) {
			continue // overlaps one already masked
		}
		b.WriteString(text[at:s.Start])
		b.WriteString("[" + string(s.Category) + "]")
		at = s.End
	}
	b.WriteString(text[at:])
	return b.String()
}
