package declarative

import (
	"bytes"
	"strings"
	"testing"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

const restrictedConfig = `api_version: ` + APIVersion + `
organizations:
  - name: clinic
    teams:
      - name: notes
        data_classification: restricted
api_keys:
  - name: scribe
    organization: clinic
    team: notes
    policy:
      data_classification: restricted
`

func TestPlanDataClassification(t *testing.T) {
	c := mustParse(t, restrictedConfig)
	p, err := Build(c, emptyState(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	p.Write(&buf)
	if !strings.Contains(buf.String(), "data classification restricted") {
		t.Fatalf("plan:\n%s", buf.String())
	}
	if d := policyDiff(&pb.APIKeyPolicy{}, c.APIKeys[0].Policy); len(d) != 1 || !strings.Contains(d[0], `"restricted"`) {
		t.Fatalf("policy diff %v", d)
	}
	for _, bad := range []string{
		strings.Replace(restrictedConfig, "data_classification: restricted\napi_keys", "data_classification: secret\napi_keys", 1),
		strings.Replace(restrictedConfig, "policy:\n      data_classification: restricted", "policy:\n      data_classification: public", 1),
	} {
		c, err := Parse([]byte(bad))
		if err == nil {
			err = c.Validate()
		}
		if err == nil || !strings.Contains(err.Error(), "data_classification") {
			t.Fatalf("accepted:\n%s\nerr %v", bad, err)
		}
	}
}

func TestLifted(t *testing.T) {
	for _, c := range [][3]string{{"", "", ""}, {"restricted", "restricted", "restricted"}, {"restricted", "", "unrestricted"}, {"", "restricted", "restricted"}} {
		if got := lifted(c[0], c[1]); got != c[2] {
			t.Errorf("lifted(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}
