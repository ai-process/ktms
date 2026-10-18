package guardrails

import (
	"context"
	"fmt"
	"strings"

	"github.com/ai-process/ktms/internal/llm"
	"github.com/rs/zerolog/log"
)

// Guardrails coordinates PII masking and prompt injection safety checks.
type Guardrails struct {
	Enabled               bool
	PIIAction             Action
	PromptInjectionAction Action
	// Latent, when set, screens text in representation space after the regex checks.
	Latent       *LatentDetector
	LatentAction Action
}

// latentBatchSize bounds one encoder call when screening many texts at once.
const latentBatchSize = 256

// New creates a configured Guardrails instance.
func New(enabled bool, piiAction, injectionAction string) *Guardrails {
	pii := Action(strings.ToLower(piiAction))
	if pii != ActionRedact {
		pii = ActionBlock
	}
	inj := Action(strings.ToLower(injectionAction))
	if inj != ActionLog {
		inj = ActionBlock
	}
	return &Guardrails{
		Enabled:               enabled,
		PIIAction:             pii,
		PromptInjectionAction: inj,
	}
}

// WithLatent adds representation-space screening; action is block or log.
func (g *Guardrails) WithLatent(d *LatentDetector, action string) *Guardrails {
	g.Latent = d
	g.LatentAction = ActionBlock
	if Action(strings.ToLower(action)) == ActionLog {
		g.LatentAction = ActionLog
	}
	return g
}

// InspectText validates a standalone text string against security policies.
func (g *Guardrails) InspectText(ctx context.Context, text string, vault *TokenVault) (string, []Violation, error) {
	if !g.Enabled || text == "" {
		return text, nil, nil
	}
	out, vs, err := g.inspectText(text, vault)
	if err != nil {
		return "", vs, err
	}
	lvs, _, err := g.screenLatent(ctx, []string{out})
	vs = append(vs, lvs...)
	if err != nil {
		return "", vs, err
	}
	return out, vs, nil
}

// InspectTexts screens many independent texts without redaction, batching the
// latent encoder calls; batch intake would otherwise pay one round trip per
// text. On a block it also returns the index of the offending text.
func (g *Guardrails) InspectTexts(ctx context.Context, texts []string) (int, []Violation, error) {
	if !g.Enabled {
		return -1, nil, nil
	}
	var all []Violation
	for i, t := range texts {
		if t == "" {
			continue
		}
		_, vs, err := g.inspectText(t, nil)
		all = append(all, vs...)
		if err != nil {
			return i, all, err
		}
	}
	for start := 0; start < len(texts); start += latentBatchSize {
		end := min(start+latentBatchSize, len(texts))
		vs, bad, err := g.screenLatent(ctx, texts[start:end])
		all = append(all, vs...)
		if err != nil {
			if bad >= 0 {
				bad += start
			}
			return bad, all, err
		}
	}
	return -1, all, nil
}

// screenLatent fails open when the encoder is unreachable: the regex checks
// already ran, and an encoder outage must not take the whole proxy down.
// The int is the index of the text that caused a block, or -1.
func (g *Guardrails) screenLatent(ctx context.Context, texts []string) ([]Violation, int, error) {
	if g.Latent == nil || len(texts) == 0 {
		return nil, -1, nil
	}
	matches, err := g.Latent.Classify(ctx, texts)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, -1, ctxErr
		}
		log.Warn().Err(err).Int("texts", len(texts)).Msg("guardrail: latent screening unavailable, skipped")
		return nil, -1, nil
	}
	if len(matches) == 0 {
		return nil, -1, nil
	}
	vs := make([]Violation, 0, len(matches))
	for _, m := range matches {
		vs = append(vs, Violation{
			Category: CategoryLatentThreat,
			Detail:   fmt.Sprintf("latent match %s (similarity %.3f)", m.Category, m.Score),
			Value:    m.Category,
		})
	}
	if g.LatentAction == ActionBlock {
		return vs, matches[0].Index, fmt.Errorf("%w: latent %s intent detected", ErrGuardrailViolation, matches[0].Category)
	}
	for _, m := range matches {
		log.Warn().Str("category", m.Category).Float32("score", m.Score).Msg("guardrail: latent threat detected (logged)")
	}
	return vs, -1, nil
}

// inspectText runs the regex and PII checks on one text.
func (g *Guardrails) inspectText(text string, vault *TokenVault) (string, []Violation, error) {

	var allViolations []Violation

	// 1. Prompt Injection check
	if detected, pattern := DetectPromptInjection(text); detected {
		v := Violation{
			Category: CategoryPromptInjection,
			Detail:   "prompt injection heuristic match: " + pattern,
			Value:    pattern,
		}
		allViolations = append(allViolations, v)
		if g.PromptInjectionAction == ActionBlock {
			return "", allViolations, fmt.Errorf("%w: prompt injection detected", ErrGuardrailViolation)
		}
		log.Warn().Str("pattern", pattern).Msg("guardrail: prompt injection detected (logged)")
	}

	// 2. PII check
	if g.PIIAction == ActionBlock {
		spans := DetectPII(text)
		if len(spans) > 0 {
			var cats []string
			for _, s := range spans {
				cats = append(cats, string(s.Category))
				allViolations = append(allViolations, Violation{
					Category: s.Category,
					Detail:   "blocked " + string(s.Category),
					Value:    s.Value,
				})
			}
			return "", allViolations, fmt.Errorf("%w: PII detected (%s)", ErrGuardrailViolation, strings.Join(cats, ", "))
		}
		return text, allViolations, nil
	}

	// PIIAction == ActionRedact
	if vault != nil {
		redacted, piiViolations := vault.Redact(text)
		allViolations = append(allViolations, piiViolations...)
		return redacted, allViolations, nil
	}

	return text, allViolations, nil
}

// InspectPII checks or redacts PII without applying prompt injection heuristics.
func (g *Guardrails) InspectPII(text string, vault *TokenVault) (string, []Violation, error) {
	if !g.Enabled || text == "" {
		return text, nil, nil
	}

	var allViolations []Violation
	if g.PIIAction == ActionBlock {
		spans := DetectPII(text)
		if len(spans) > 0 {
			var cats []string
			for _, s := range spans {
				cats = append(cats, string(s.Category))
				allViolations = append(allViolations, Violation{
					Category: s.Category,
					Detail:   "blocked " + string(s.Category),
					Value:    s.Value,
				})
			}
			return "", allViolations, fmt.Errorf("%w: PII detected (%s)", ErrGuardrailViolation, strings.Join(cats, ", "))
		}
		return text, allViolations, nil
	}

	if g.PIIAction == ActionRedact && vault != nil {
		redacted, piiViolations := vault.Redact(text)
		allViolations = append(allViolations, piiViolations...)
		return redacted, allViolations, nil
	}

	return text, allViolations, nil
}

// InspectChatContext validates and sanitizes instructions and messages in a
// ChatContext. Latent screening runs once over every text, after redaction.
func (g *Guardrails) InspectChatContext(ctx context.Context, chat *llm.ChatContext, vault *TokenVault) ([]Violation, error) {
	if !g.Enabled || chat == nil {
		return nil, nil
	}

	var allViolations []Violation
	var screened []string

	// Inspect system instructions
	if base := chat.GetBaseSystemInstruction(); base != "" {
		redacted, vs, err := g.inspectText(base, vault)
		if err != nil {
			return vs, err
		}
		allViolations = append(allViolations, vs...)
		screened = append(screened, redacted)
		if g.PIIAction == ActionRedact {
			chat.SetBaseSystemInstruction(redacted)
		}
	}

	if reqInst := chat.GetRequestInstruction(); reqInst != "" {
		redacted, vs, err := g.inspectText(reqInst, vault)
		if err != nil {
			return vs, err
		}
		allViolations = append(allViolations, vs...)
		screened = append(screened, redacted)
		if g.PIIAction == ActionRedact {
			chat.SetRequestInstruction(redacted)
		}
	}

	// Inspect all messages and text parts
	var inspectErr error
	chat.RewriteMessages(func(m *llm.Message) {
		if inspectErr != nil || m == nil {
			return
		}
		if m.Text != "" {
			redacted, vs, err := g.inspectText(m.Text, vault)
			allViolations = append(allViolations, vs...)
			if err != nil {
				inspectErr = err
				return
			}
			screened = append(screened, redacted)
			if g.PIIAction == ActionRedact {
				m.Text = redacted
			}
		}
		for _, p := range m.Parts {
			if p != nil && p.Type == llm.ContentPartTypeText && p.Text != "" {
				redacted, vs, err := g.inspectText(p.Text, vault)
				allViolations = append(allViolations, vs...)
				if err != nil {
					inspectErr = err
					return
				}
				screened = append(screened, redacted)
				if g.PIIAction == ActionRedact {
					p.Text = redacted
				}
			}
		}
	})

	if inspectErr != nil {
		return allViolations, inspectErr
	}
	vs, _, err := g.screenLatent(ctx, screened)
	allViolations = append(allViolations, vs...)
	if err != nil {
		return allViolations, err
	}
	return allViolations, nil
}
