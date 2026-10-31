package declarative

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
)

// KeyScopes are the scopes a client key in the file may carry.
var KeyScopes = []string{apikeys.ScopeGenerate, apikeys.ScopeMCP, apikeys.ScopeAdmin, apikeys.ScopeAdminRead}

var efforts = []pb.Effort{pb.Effort_EFFORT_LOW, pb.Effort_EFFORT_MEDIUM, pb.Effort_EFFORT_HIGH}

// Validate checks what the file can show on its own: names and ids unique,
// references inside the file resolved, a catch-all rule for every effort.
// Keystone validates the full configuration again when it is applied.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	models := map[string]bool{}
	for i, m := range c.Models {
		switch {
		case m.GetId() == "":
			bad("models[%d]: id is required", i)
		case models[m.GetId()]:
			bad("models[%d]: duplicate id %q", i, m.GetId())
		}
		models[m.GetId()] = true
		if m.GetVendor() == "" {
			bad("models[%d] %s: vendor is required", i, m.GetId())
		}
	}

	if c.Manages("rules") {
		names, prios := map[string]bool{}, map[int32]string{}
		covered := map[pb.Effort]bool{}
		for i, r := range c.Rules {
			if r.GetName() == "" {
				bad("rules[%d]: name is required", i)
			} else if names[r.GetName()] {
				bad("rules[%d]: duplicate name %q", i, r.GetName())
			}
			names[r.GetName()] = true
			if other, dup := prios[r.GetPriority()]; dup {
				bad("rules[%d] %s: priority %d is also %s's", i, r.GetName(), r.GetPriority(), other)
			}
			prios[r.GetPriority()] = r.GetName()
			if len(r.GetUse()) == 0 {
				bad("rules[%d] %s: use needs at least one model", i, r.GetName())
			}
			inChain := map[string]bool{}
			for _, id := range r.GetUse() {
				if inChain[id] {
					bad("rules[%d] %s: model %q twice in use", i, r.GetName(), id)
				}
				inChain[id] = true
			}
			if c.Manages("models") {
				for _, id := range append(append([]string{}, r.GetUse()...), r.GetShadowModel()) {
					if id != "" && !models[id] {
						bad("rules[%d] %s: model %q is not in models", i, r.GetName(), id)
					}
				}
			}
			if len(r.GetWeights()) > 0 && len(r.GetWeights()) != len(r.GetUse()) {
				bad("rules[%d] %s: weights needs one entry per model in use", i, r.GetName())
			}
			if r.GetEnabled() && len(r.GetAttributes()) == 0 {
				if r.GetEffort() == pb.Effort_EFFORT_UNSPECIFIED {
					for _, e := range efforts {
						covered[e] = true
					}
				} else {
					covered[r.GetEffort()] = true
				}
			}
		}
		// As in Keystone: once any rule exists, every effort needs a catch-all.
		for _, e := range efforts {
			if len(c.Rules) > 0 && !covered[e] {
				bad("rules: no enabled catch-all rule (no attributes) for %s", e)
			}
		}
	}

	orgs, teams := map[string]bool{}, map[string]bool{}
	for i, o := range c.Organizations {
		if o.Name == "" {
			bad("organizations[%d]: name is required", i)
		} else if orgs[o.Name] {
			bad("organizations[%d]: duplicate name %q", i, o.Name)
		}
		orgs[o.Name] = true
		seen := map[string]bool{}
		for j, t := range o.Teams {
			if t.Name == "" {
				bad("organizations[%d].teams[%d]: name is required", i, j)
			} else if seen[t.Name] {
				bad("organizations[%d].teams[%d]: duplicate team %q", i, j, t.Name)
			}
			seen[t.Name] = true
			teams[o.Name+"/"+t.Name] = true
			if t.MonthlyBudgetUSD < 0 {
				bad("organizations[%d].teams[%d] %s: monthly_budget_usd must be >= 0", i, j, t.Name)
			}
			// Normalised here, so plan compares the file with what the server stores.
			if v, err := apikeys.ParseDataClassification(t.DataClassification, false); err != nil {
				bad("organizations[%d].teams[%d] %s: %v", i, j, t.Name, err)
			} else {
				c.Organizations[i].Teams[j].DataClassification = v
			}
		}
	}

	keys := map[string]bool{}
	for i, k := range c.APIKeys {
		if k.Name == "" {
			bad("api_keys[%d]: name is required", i)
		} else if keys[k.Name] {
			bad("api_keys[%d]: duplicate name %q", i, k.Name)
		}
		keys[k.Name] = true
		for _, s := range k.Scopes {
			if !slices.Contains(KeyScopes, s) {
				bad("api_keys[%d] %s: scope %q is not one of %s", i, k.Name, s, strings.Join(KeyScopes, ", "))
			}
		}
		if slices.Contains(k.Scopes, apikeys.ScopeAdminRead) && (k.Organization != "" || k.Team != "") {
			bad("api_keys[%d] %s: an admin:read key reads every tenant and cannot belong to an organization or team", i, k.Name)
		}
		if k.Team != "" && k.Organization == "" {
			bad("api_keys[%d] %s: team %q needs its organization", i, k.Name, k.Team)
		}
		if c.Manages("organizations") {
			if k.Organization != "" && !orgs[k.Organization] {
				bad("api_keys[%d] %s: organization %q is not in organizations", i, k.Name, k.Organization)
			}
			if k.Team != "" && !teams[k.Organization+"/"+k.Team] {
				bad("api_keys[%d] %s: team %q is not in organization %q", i, k.Name, k.Team, k.Organization)
			}
		}
		if p := k.Policy; p != nil && (p.GetRpmLimit() < 0 || p.GetTpmLimit() < 0) {
			bad("api_keys[%d] %s: rpm_limit and tpm_limit must be >= 0", i, k.Name)
		}
		if v, err := apikeys.ParseDataClassification(k.Policy.GetDataClassification(), false); err != nil {
			bad("api_keys[%d] %s: %v", i, k.Name, err)
		} else if k.Policy != nil {
			k.Policy.DataClassification = v
		}
	}

	vk := map[string]bool{}
	for i, v := range c.VendorKeys {
		id := v.APIKeyName + "/" + v.Vendor
		switch {
		case v.APIKeyName == "" || v.Vendor == "":
			bad("vendor_keys[%d]: api_key_name and vendor are required", i)
		case v.PlainKeyEnv == "":
			bad("vendor_keys[%d] %s: plain_key_env names the variable holding the key", i, id)
		case vk[id]:
			bad("vendor_keys[%d]: duplicate %s", i, id)
		}
		vk[id] = true
		if c.Manages("api_keys") && !keys[v.APIKeyName] {
			bad("vendor_keys[%d]: api key %q is not in api_keys", i, v.APIKeyName)
		}
	}

	sl := map[string]bool{}
	for i, l := range c.SpendLimits {
		id := l.SubjectType + "/" + l.SubjectID + "/" + l.Period
		switch {
		case l.SubjectType == "team":
			bad("spend_limits[%d]: a team's cap is its monthly_budget_usd in organizations", i)
		case l.SubjectType != "key" && l.SubjectType != "user":
			bad("spend_limits[%d]: subject_type must be key or user", i)
		case l.SubjectID == "":
			bad("spend_limits[%d]: subject_id is required (* for every subject)", i)
		case l.Period != "day" && l.Period != "month":
			bad("spend_limits[%d]: period must be day or month", i)
		case l.Nano() <= 0:
			bad("spend_limits[%d] %s: set limit_usd or limit_nanousd above 0", i, id)
		case l.LimitUSD > 0 && l.LimitNanoUSD > 0:
			bad("spend_limits[%d] %s: set limit_usd or limit_nanousd, not both", i, id)
		case sl[id]:
			bad("spend_limits[%d]: duplicate %s", i, id)
		}
		sl[id] = true
	}

	servers := map[string]bool{}
	for i, m := range c.MCPServers {
		switch {
		case m.GetName() == "" || m.GetUrl() == "":
			bad("mcp_servers[%d]: name and url are required", i)
		case servers[m.GetName()]:
			bad("mcp_servers[%d]: duplicate name %q", i, m.GetName())
		}
		servers[m.GetName()] = true
	}
	creds := map[string]bool{}
	for i, mc := range c.MCPCredentials {
		switch {
		case mc.Server == "" || mc.ValueEnv == "":
			bad("mcp_credentials[%d]: server and value_env are required", i)
		case creds[mc.Server]:
			bad("mcp_credentials[%d]: duplicate server %q", i, mc.Server)
		case c.Manages("mcp_servers") && !servers[mc.Server]:
			bad("mcp_credentials[%d]: server %q is not in mcp_servers", i, mc.Server)
		}
		creds[mc.Server] = true
	}

	ur := map[string]bool{}
	for i, u := range c.UserRateLimits {
		id := u.GetUserId() + "/" + u.GetModelId()
		if u.GetUserId() == "" || u.GetModelId() == "" {
			bad("user_rate_limits[%d]: user_id and model_id are required (* for every one)", i)
		} else if ur[id] {
			bad("user_rate_limits[%d]: duplicate %s", i, id)
		}
		ur[id] = true
	}
	return joinErrors(errs)
}

func joinErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	lines := make([]string, len(errs))
	for i, e := range errs {
		lines[i] = "  " + e.Error()
	}
	return errors.New("invalid configuration:\n" + strings.Join(lines, "\n"))
}
