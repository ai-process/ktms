package guardrails

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// TokenVault stores reversible anonymization mappings for a single session.
type TokenVault struct {
	mu            sync.Mutex
	toPlaceholder map[string]string
	toOriginal    map[string]string
	counts        map[ViolationCategory]int
}

// NewTokenVault initializes an empty vault.
func NewTokenVault() *TokenVault {
	return &TokenVault{
		toPlaceholder: make(map[string]string),
		toOriginal:    make(map[string]string),
		counts:        make(map[ViolationCategory]int),
	}
}

// GetOrCreate generates or returns an existing placeholder for the original string.
func (v *TokenVault) GetOrCreate(category ViolationCategory, original string) string {
	v.mu.Lock()
	defer v.mu.Unlock()

	if ph, ok := v.toPlaceholder[original]; ok {
		return ph
	}

	v.counts[category]++
	ph := fmt.Sprintf("[%s_%d]", strings.ToUpper(string(category)), v.counts[category])
	v.toPlaceholder[original] = ph
	v.toOriginal[ph] = original
	return ph
}

// Desanitize replaces placeholders with original sensitive values in plain text.
func (v *TokenVault) Desanitize(text string) string {
	v.mu.Lock()
	defer v.mu.Unlock()

	if len(v.toOriginal) == 0 || !strings.Contains(text, "[") {
		return text
	}

	for ph, orig := range v.toOriginal {
		text = strings.ReplaceAll(text, ph, orig)
	}
	return text
}

// DesanitizeJSON replaces placeholders in JSON payloads, escaping replacement strings.
func (v *TokenVault) DesanitizeJSON(jsonText string) string {
	v.mu.Lock()
	defer v.mu.Unlock()

	if len(v.toOriginal) == 0 || !strings.Contains(jsonText, "[") {
		return jsonText
	}

	for ph, orig := range v.toOriginal {
		escapedBytes, err := json.Marshal(orig)
		if err == nil && len(escapedBytes) >= 2 {
			// Strip outer quotes to embed inside existing JSON string fields
			escapedStr := string(escapedBytes[1 : len(escapedBytes)-1])
			jsonText = strings.ReplaceAll(jsonText, ph, escapedStr)
		} else {
			jsonText = strings.ReplaceAll(jsonText, ph, orig)
		}
	}
	return jsonText
}

// StreamDesanitizer buffers partial placeholder sequences across stream chunks.
type StreamDesanitizer struct {
	vault  *TokenVault
	buffer string
}

// NewStreamDesanitizer constructs a streaming desanitizer tied to a token vault.
func NewStreamDesanitizer(v *TokenVault) *StreamDesanitizer {
	return &StreamDesanitizer{vault: v}
}

// ProcessChunk transforms an incremental stream delta, buffering incomplete placeholders.
func (s *StreamDesanitizer) ProcessChunk(delta string) string {
	if s.vault == nil {
		return delta
	}
	combined := s.buffer + delta
	lastOpen := strings.LastIndex(combined, "[")
	if lastOpen != -1 && strings.LastIndex(combined, "]") < lastOpen {
		// Incomplete placeholder sequence at the tail
		ready := combined[:lastOpen]
		s.buffer = combined[lastOpen:]
		return s.vault.Desanitize(ready)
	}

	s.buffer = ""
	return s.vault.Desanitize(combined)
}

// Flush returns any trailing buffered text.
func (s *StreamDesanitizer) Flush() string {
	if s.buffer == "" {
		return ""
	}
	res := s.vault.Desanitize(s.buffer)
	s.buffer = ""
	return res
}

// Redact replaces PII spans in text with vault placeholders.
func (v *TokenVault) Redact(text string) (string, []Violation) {
	spans := DetectPII(text)
	if len(spans) == 0 {
		return text, nil
	}

	// Sort spans by start index descending to safely replace in-place
	sort.Slice(spans, func(i, j int) bool {
		return spans[i].Start > spans[j].Start
	})

	var violations []Violation
	res := text
	for _, span := range spans {
		ph := v.GetOrCreate(span.Category, span.Value)
		res = res[:span.Start] + ph + res[span.End:]
		violations = append(violations, Violation{
			Category: span.Category,
			Detail:   fmt.Sprintf("masked %s", span.Category),
			Value:    span.Value,
		})
	}
	return res, violations
}
