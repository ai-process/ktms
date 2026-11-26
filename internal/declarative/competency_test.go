package declarative

import (
	"bytes"
	"strings"
	"testing"
)

const competent = `api_version: ktms.ai-process.dev/v1alpha1
models:
  - id: small
    vendor: openai
    enabled: true
    competencies: {general: basic}
  - id: med
    vendor: openai
    enabled: true
    competencies: {general: proficient, medical: expert}
rules:
  - name: medical
    priority: 5
    enabled: true
    needs: {medical: proficient}
    use: [med]
`

func TestCompetenciesNeedNoCatchAll(t *testing.T) {
	c := mustParse(t, competent)
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	_, err := Parse([]byte(strings.Replace(competent, "medical: expert", "medical: guru", 1)))
	if err == nil || !strings.Contains(err.Error(), "unknown level") {
		t.Fatalf("bad level: err = %v", err)
	}
}

func TestPlanHintsAtEfforts(t *testing.T) {
	c := mustParse(t, example)
	p, err := Build(c, liveOf(t, c, "sk-1"), Options{Env: env(map[string]string{"TEST_OPENAI_KEY": "sk-1"})})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	p.Write(&buf)
	for _, want := range []string{
		"hint: model gpt-4o-mini: write competencies: {general: expert}",
		"hint: rule high: write needs: {general: expert}",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("plan lacks %q:\n%s", want, buf.String())
		}
	}
	if !p.Empty() {
		t.Errorf("hints are no change:\n%s", buf.String())
	}
}
