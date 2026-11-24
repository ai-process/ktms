package guardrails

import "regexp"

// Names of the prompt injection heuristics, as DetectPromptInjection reports them.
const (
	InjectionIgnoreInstructions = "ignore_instructions"
	InjectionModeSwitch         = "mode_switch"
	InjectionPromptLeak         = "prompt_leak"
)

var injectionRules = []struct {
	name string
	re   *regexp.Regexp
}{
	{InjectionIgnoreInstructions, regexp.MustCompile(`(?i)\b(ignore|disregard|forget|bypass|override)\b.*?\b(previous|prior|above|system|all)\b.*?\b(instructions|prompts|rules|commands|context|directives)\b`)},
	{InjectionModeSwitch, regexp.MustCompile(`(?i)\b(developer mode|jailbreak|dan mode|do anything now|system override|unrestricted mode)\b`)},
	{InjectionPromptLeak, regexp.MustCompile(`(?i)\b(repeat|reveal|print|display|output|show)\b.*?\b(system|initial|base)\b.*?\b(prompt|instruction|instructions|message)\b`)},
}

// DetectPromptInjection checks whether the given prompt attempts jailbreak or
// instruction override, and names the rule that matched. It never returns the
// matched text: a match can span the whole message, secrets included.
func DetectPromptInjection(text string) (bool, string) {
	for _, r := range injectionRules {
		if r.re.MatchString(text) {
			return true, r.name
		}
	}
	return false, ""
}
