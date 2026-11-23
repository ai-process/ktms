package registry

import (
	"strings"

	"github.com/ai-process/ktms/internal/proxydb"
)

// AnyLanguage in a model's languages matches every tag.
const AnyLanguage = "*"

// ValidLanguageTag accepts "*" and BCP-47-shaped tags: letters, digits and
// hyphens, a 2-3 letter primary subtag. Full registry validation is the
// engine's job; this catches typos such as "german" or "de_DE".
func ValidLanguageTag(tag string) bool {
	if tag == AnyLanguage {
		return true
	}
	parts := strings.Split(tag, "-")
	if n := len(parts[0]); n < 2 || n > 3 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 8 {
			return false
		}
		for _, r := range p {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
				return false
			}
		}
	}
	return true
}

// ServesLanguagePair reports whether m declares the pair. An empty source in
// the request matches any: the engine detects it. A declared tag covers its
// longer variants; an empty list (never on a valid translate model) is any.
func ServesLanguagePair(m *proxydb.Model, source, target string) bool {
	return languageListed(m.LanguagesSource, source) && languageListed(m.LanguagesTarget, target)
}

func languageListed(list []string, tag string) bool {
	if len(list) == 0 || tag == "" {
		return true
	}
	// RFC 4647 basic filtering: "zh-Hant" covers "zh-Hant-TW", "de" covers "de-AT".
	for _, l := range list {
		if l == AnyLanguage || strings.EqualFold(l, tag) ||
			(len(tag) > len(l) && tag[len(l)] == '-' && strings.EqualFold(l, tag[:len(l)])) {
			return true
		}
	}
	return false
}
