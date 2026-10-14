package guardrails

import "regexp"

var (
	injectionIgnoreRe = regexp.MustCompile(`(?i)\b(ignore|disregard|forget|bypass|override)\b.*?\b(previous|prior|above|system|all)\b.*?\b(instructions|prompts|rules|commands|context|directives)\b`)
	injectionModesRe  = regexp.MustCompile(`(?i)\b(developer mode|jailbreak|dan mode|do anything now|system override|unrestricted mode)\b`)
	injectionLeakRe   = regexp.MustCompile(`(?i)\b(repeat|reveal|print|display|output|show)\b.*?\b(system|initial|base)\b.*?\b(prompt|instruction|instructions|message)\b`)
)

// DetectPromptInjection checks whether the given prompt attempts jailbreak or instruction override.
func DetectPromptInjection(text string) (bool, string) {
	if loc := injectionIgnoreRe.FindString(text); loc != "" {
		return true, loc
	}
	if loc := injectionModesRe.FindString(text); loc != "" {
		return true, loc
	}
	if loc := injectionLeakRe.FindString(text); loc != "" {
		return true, loc
	}
	return false, ""
}
