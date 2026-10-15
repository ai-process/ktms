package guardrails

import (
	"context"
	"fmt"
	"strings"

	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/rs/zerolog/log"
)

// Guardrails coordinates PII masking and prompt injection safety checks.
type Guardrails struct {
	Enabled               bool
	PIIAction             Action
	PromptInjectionAction Action
}

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

// InspectText validates a standalone text string against security policies.
func (g *Guardrails) InspectText(text string, vault *TokenVault) (string, []Violation, error) {
	if !g.Enabled || text == "" {
		return text, nil, nil
	}

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

// InspectChatContext validates and sanitizes instructions and messages in a ChatContext.
func (g *Guardrails) InspectChatContext(ctx context.Context, chat *llm.ChatContext, vault *TokenVault) ([]Violation, error) {
	if !g.Enabled || chat == nil {
		return nil, nil
	}

	var allViolations []Violation

	// Inspect system instructions
	if base := chat.GetBaseSystemInstruction(); base != "" {
		redacted, vs, err := g.InspectText(base, vault)
		if err != nil {
			return vs, err
		}
		allViolations = append(allViolations, vs...)
		if g.PIIAction == ActionRedact {
			chat.SetBaseSystemInstruction(redacted)
		}
	}

	if reqInst := chat.GetRequestInstruction(); reqInst != "" {
		redacted, vs, err := g.InspectText(reqInst, vault)
		if err != nil {
			return vs, err
		}
		allViolations = append(allViolations, vs...)
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
			redacted, vs, err := g.InspectText(m.Text, vault)
			if err != nil {
				inspectErr = err
				allViolations = append(allViolations, vs...)
				return
			}
			allViolations = append(allViolations, vs...)
			if g.PIIAction == ActionRedact {
				m.Text = redacted
			}
		}
		for _, p := range m.Parts {
			if p != nil && p.Type == llm.ContentPartTypeText && p.Text != "" {
				redacted, vs, err := g.InspectText(p.Text, vault)
				if err != nil {
					inspectErr = err
					allViolations = append(allViolations, vs...)
					return
				}
				allViolations = append(allViolations, vs...)
				if g.PIIAction == ActionRedact {
					p.Text = redacted
				}
			}
		}
	})

	if inspectErr != nil {
		return allViolations, inspectErr
	}
	return allViolations, nil
}
