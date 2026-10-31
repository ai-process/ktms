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

// The file's spelling is normalised on load, so a plan after apply compares
// "restricted" with what the server stored and stays empty.
func TestDataClassificationNormalised(t *testing.T) {
	c := mustParse(t, strings.ReplaceAll(restrictedConfig, "data_classification: restricted", "data_classification: Restricted"))
	if got := c.Organizations[0].Teams[0].DataClassification; got != "restricted" {
		t.Fatalf("team %q", got)
	}
	if got := c.APIKeys[0].Policy.GetDataClassification(); got != "restricted" {
		t.Fatalf("key %q", got)
	}
	if d := policyDiff(&pb.APIKeyPolicy{DataClassification: "restricted"}, c.APIKeys[0].Policy); len(d) != 0 {
		t.Fatalf("policy diff %v", d)
	}
}
