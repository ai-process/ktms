// Package declarative keeps a Keystone configuration in a file and applies it
// through the Admin API. Models, rules and user rate limits are the Admin API's
// own messages, so the format follows ktms.v1 field for field.
package declarative

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

// APIVersion names this format; it may still change within 1.x.
const APIVersion = "ktms.ai-process.dev/v1alpha1"

// Config is a desired configuration. A nil section is not managed: apply
// leaves that part of Keystone alone. A present one, even empty, is managed,
// and --prune deletes what it does not list.
type Config struct {
	APIVersion     string
	Models         []*pb.ModelSpec
	Rules          []*pb.RuleSpec
	Organizations  []Organization
	APIKeys        []APIKey
	VendorKeys     []VendorKey
	SpendLimits    []SpendLimit
	UserRateLimits []*pb.UserRateLimitSpec

	// Which sections the file has, since an empty list differs from a missing one.
	managed map[string]bool
}

// Organization groups teams; both are matched by name.
type Organization struct {
	Name  string `json:"name"`
	Teams []Team `json:"teams,omitempty"`
}

// Team is a group of keys with a monthly budget and a model allowlist.
type Team struct {
	Name             string   `json:"name"`
	MonthlyBudgetUSD float64  `json:"monthly_budget_usd,omitempty"`
	AllowedModels    []string `json:"allowed_models,omitempty"`
}

// APIKey is a client key, matched by name. Its secret exists only in the
// apply that mints it.
type APIKey struct {
	Name         string           `json:"name"`
	Scopes       []string         `json:"scopes,omitempty"`
	Organization string           `json:"organization,omitempty"`
	Team         string           `json:"team,omitempty"`
	Policy       *pb.APIKeyPolicy `json:"-"`
}

// VendorKey assigns a vendor credential to a client key. The credential is
// read from an environment variable, never from the file.
type VendorKey struct {
	APIKeyName  string `json:"api_key_name"`
	Vendor      string `json:"vendor"`
	PlainKeyEnv string `json:"plain_key_env"`
}

// SpendLimit caps a key's or a user's spend per UTC day or month. Team caps
// belong to Team.MonthlyBudgetUSD.
type SpendLimit struct {
	SubjectType  string  `json:"subject_type"`
	SubjectID    string  `json:"subject_id"`
	Period       string  `json:"period"`
	LimitUSD     float64 `json:"limit_usd,omitempty"`
	LimitNanoUSD int64   `json:"limit_nanousd,omitempty"`
}

// Nano is the cap in nano-USD, whichever of the two fields the file used.
func (l SpendLimit) Nano() int64 {
	if l.LimitNanoUSD > 0 {
		return l.LimitNanoUSD
	}
	return int64(math.Round(l.LimitUSD * 1e9))
}

// Manages reports whether the file has the section (models, rules,
// organizations, api_keys, vendor_keys, spend_limits, user_rate_limits).
func (c *Config) Manages(section string) bool { return c.managed[section] }

var sections = []string{"models", "rules", "organizations", "api_keys", "vendor_keys", "spend_limits", "user_rate_limits"}

// Load reads and parses a configuration file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse reads YAML (or JSON) into a Config and checks it on its own; checks
// that need the live configuration happen in Plan.
func Parse(b []byte) (*Config, error) {
	var doc map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty configuration")
		}
		return nil, fmt.Errorf("yaml: %w", err)
	}
	cfg := &Config{managed: map[string]bool{}}
	for k := range doc {
		if k == "api_version" {
			continue
		}
		known := false
		for _, s := range sections {
			known = known || k == s
		}
		if !known {
			return nil, fmt.Errorf("unknown section %q (known: api_version, %s)", k, strings.Join(sections, ", "))
		}
		cfg.managed[k] = true
	}
	v, _ := doc["api_version"].(string)
	if v != APIVersion {
		return nil, fmt.Errorf("api_version must be %q, got %q", APIVersion, v)
	}
	cfg.APIVersion = v

	var err error
	if cfg.Models, err = protoList(doc, "models", func() *pb.ModelSpec { return &pb.ModelSpec{} }); err != nil {
		return nil, err
	}
	if cfg.Rules, err = protoList(doc, "rules", func() *pb.RuleSpec { return &pb.RuleSpec{} }); err != nil {
		return nil, err
	}
	if cfg.UserRateLimits, err = protoList(doc, "user_rate_limits", func() *pb.UserRateLimitSpec { return &pb.UserRateLimitSpec{} }); err != nil {
		return nil, err
	}
	if err := jsonList(doc, "organizations", &cfg.Organizations); err != nil {
		return nil, err
	}
	if err := jsonList(doc, "vendor_keys", &cfg.VendorKeys); err != nil {
		return nil, err
	}
	if err := jsonList(doc, "spend_limits", &cfg.SpendLimits); err != nil {
		return nil, err
	}
	if err := parseKeys(doc, cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// protoList decodes a section of Admin API messages. A missing "enabled" means
// true: a model or rule written down is meant to be used.
func protoList[T proto.Message](doc map[string]any, section string, mk func() T) ([]T, error) {
	raw, ok := doc[section]
	if !ok {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok && raw != nil {
		return nil, fmt.Errorf("%s: must be a list", section)
	}
	out := make([]T, 0, len(items))
	for i, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s[%d]: must be a mapping", section, i)
		}
		if section != "user_rate_limits" {
			if _, set := m["enabled"]; !set {
				m["enabled"] = true
			}
		}
		b, err := json.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", section, i, err)
		}
		msg := mk()
		if err := protojson.Unmarshal(b, msg); err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", section, i, err)
		}
		out = append(out, msg)
	}
	return out, nil
}

func jsonList(doc map[string]any, section string, into any) error {
	raw, ok := doc[section]
	if !ok {
		return nil
	}
	if raw == nil {
		raw = []any{}
	}
	if _, list := raw.([]any); !list {
		return fmt.Errorf("%s: must be a list", section)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", section, err)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return fmt.Errorf("%s: %w", section, err)
	}
	return nil
}

// parseKeys decodes api_keys, whose policy is an Admin API message.
func parseKeys(doc map[string]any, cfg *Config) error {
	raw, ok := doc["api_keys"]
	if !ok {
		return nil
	}
	items, ok := raw.([]any)
	if !ok && raw != nil {
		// A managed section parsed as empty would let --prune revoke every key.
		return errors.New("api_keys: must be a list")
	}
	cfg.APIKeys = make([]APIKey, 0, len(items))
	for i, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("api_keys[%d]: must be a mapping", i)
		}
		policy, hasPolicy := m["policy"]
		delete(m, "policy")
		b, _ := json.Marshal(m)
		var k APIKey
		d := json.NewDecoder(bytes.NewReader(b))
		d.DisallowUnknownFields()
		if err := d.Decode(&k); err != nil {
			return fmt.Errorf("api_keys[%d]: %w", i, err)
		}
		if hasPolicy && policy != nil {
			pbytes, _ := json.Marshal(policy)
			k.Policy = &pb.APIKeyPolicy{}
			if err := protojson.Unmarshal(pbytes, k.Policy); err != nil {
				return fmt.Errorf("api_keys[%d].policy: %w", i, err)
			}
		}
		cfg.APIKeys = append(cfg.APIKeys, k)
	}
	return nil
}
