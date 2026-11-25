package registry

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/ai-process/ktms/internal/proxydb"
)

// Levels of a competency or a need, in order: a model meets a need at its
// level or above.
const (
	LevelBasic      = "basic"
	LevelProficient = "proficient"
	LevelExpert     = "expert"

	// TopicGeneral is the topic efforts stand for: low, medium and high are
	// general at basic, proficient and expert.
	TopicGeneral = "general"
)

var levels = []string{LevelBasic, LevelProficient, LevelExpert}

var topicRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// LevelRank orders levels from 1 (basic) to 3 (expert); 0 is not a level.
func LevelRank(level string) int {
	for i, l := range levels {
		if l == level {
			return i + 1
		}
	}
	return 0
}

// ValidLevel reports whether level is basic, proficient or expert.
func ValidLevel(level string) bool { return LevelRank(level) > 0 }

// ValidTopic reports whether t is a topic name: lowercase letters, digits,
// "_" and "-", up to 64 characters. Topics are free; an unknown one is no error.
func ValidTopic(t string) bool { return topicRE.MatchString(t) }

// EffortLevel maps an effort to the general level it stands for.
func EffortLevel(effort string) string {
	switch effort {
	case EffortLow:
		return LevelBasic
	case EffortMedium:
		return LevelProficient
	case EffortHigh:
		return LevelExpert
	}
	return ""
}

// LevelEffort is EffortLevel backwards, for meta["effort"] and old clients.
func LevelEffort(level string) string {
	switch level {
	case LevelBasic:
		return EffortLow
	case LevelProficient:
		return EffortMedium
	case LevelExpert:
		return EffortHigh
	}
	return ""
}

// ValidateLevels checks a topic -> level map of a model, rule or request.
func ValidateLevels(m map[string]string) error {
	for t, l := range m {
		if !ValidTopic(t) {
			return fmt.Errorf("topic %q: want lowercase letters, digits, _ or -, up to 64", t)
		}
		if !ValidLevel(l) {
			return fmt.Errorf("topic %s: unknown level %q, want basic, proficient or expert", t, l)
		}
	}
	return nil
}

// legacyGeneral reports whether m's general topic comes from its efforts:
// such a model keeps 1.6 routing, serving exactly the efforts it lists.
func legacyGeneral(m *proxydb.Model) bool {
	_, explicit := m.Competencies[TopicGeneral]
	return !explicit && len(m.Efforts) > 0
}

// Competencies returns m's competencies with its efforts read as general at
// the highest effort listed, unless general is set explicitly.
func Competencies(m *proxydb.Model) map[string]string {
	out := make(map[string]string, len(m.Competencies)+1)
	for t, l := range m.Competencies {
		out[t] = l
	}
	if legacyGeneral(m) {
		best := 0
		for _, e := range m.Efforts {
			best = max(best, LevelRank(EffortLevel(e)))
		}
		if best > 0 {
			out[TopicGeneral] = levels[best-1]
		}
	}
	return out
}

// Meets reports whether m meets the need topic >= level. A model whose
// general comes from efforts meets a general need only at an effort it lists.
func Meets(m *proxydb.Model, topic, level string) bool {
	if topic == TopicGeneral && legacyGeneral(m) {
		return Serves(m, LevelEffort(level))
	}
	have := LevelRank(m.Competencies[topic])
	return have > 0 && have >= LevelRank(level)
}

// MeetsAll reports whether m meets every need.
func MeetsAll(m *proxydb.Model, needs map[string]string) bool {
	for t, l := range needs {
		if !Meets(m, t, l) {
			return false
		}
	}
	return true
}

// Excess sums, over the needed topics, how many levels m has above the need,
// so an expert is not spent where a basic model does; 0 for an exact fit.
func Excess(m *proxydb.Model, needs map[string]string) int {
	n := 0
	for t, l := range needs {
		if t == TopicGeneral && legacyGeneral(m) {
			continue // served exactly at that effort
		}
		if d := LevelRank(m.Competencies[t]) - LevelRank(l); d > 0 {
			n += d
		}
	}
	return n
}

// BlendedPrice is the price per million tokens selection ranks by:
// (3 x input + output) / 4 at the standard rate, as chats read more than they write.
func BlendedPrice(m *proxydb.Model) float64 {
	return (3*m.PriceInPerMtok + m.PriceOutPerMtok) / 4
}

// RuleMatchesNeeds reports whether a request's needs reach a rule's: each
// topic the rule names at least at the rule's level.
func RuleMatchesNeeds(ruleNeeds, needs map[string]string) bool {
	for t, l := range ruleNeeds {
		if LevelRank(needs[t]) < LevelRank(l) {
			return false
		}
	}
	return true
}

// RequestNeeds merges a request's needs with its deprecated effort, read as
// general unless the needs name general themselves, and validates them.
func RequestNeeds(needs map[string]string, effort string) (map[string]string, error) {
	if err := ValidateLevels(needs); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(needs)+1)
	for t, l := range needs {
		out[t] = l
	}
	if _, ok := out[TopicGeneral]; !ok && effort != "" {
		l := EffortLevel(effort)
		if l == "" {
			return nil, fmt.Errorf("unknown effort %q", effort)
		}
		out[TopicGeneral] = l
	}
	return out, nil
}

// FormatNeeds writes needs as "general=basic,medical=proficient", sorted by
// topic: the form of usage meta, X-KTMS-Needs and the auto: model name.
func FormatNeeds(needs map[string]string) string {
	keys := make([]string, 0, len(needs))
	for t := range needs {
		keys = append(keys, t)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, t := range keys {
		parts[i] = t + "=" + needs[t]
	}
	return strings.Join(parts, ",")
}

// ParseNeeds reads FormatNeeds' form; spaces around items are allowed.
func ParseNeeds(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		t, l, ok := strings.Cut(item, "=")
		if !ok {
			return nil, fmt.Errorf("need %q: want topic=level", item)
		}
		t, l = strings.ToLower(strings.TrimSpace(t)), strings.ToLower(strings.TrimSpace(l))
		if _, dup := out[t]; dup {
			return nil, fmt.Errorf("topic %s given twice", t)
		}
		out[t] = l
	}
	if err := ValidateLevels(out); err != nil {
		return nil, err
	}
	return out, nil
}
