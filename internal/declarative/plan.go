package declarative

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/crypto"
)

// Op is what a change does to one object.
type Op string

const (
	Create Op = "create"
	Update Op = "update"
	Delete Op = "delete"
)

// Change is one line of a plan, safe to show: it never holds a secret.
type Change struct {
	Op      Op
	Kind    string
	Name    string
	Details []string
}

// Plan is the ordered set of Admin API calls that turns the live
// configuration into the desired one.
type Plan struct {
	Changes  []Change
	Warnings []string
	steps    []step
}

type step struct {
	change Change
	run    func(ctx context.Context, c pb.AdminServiceClient, a *applied) error
}

// applied carries what earlier steps created, for the steps that refer to it.
type applied struct {
	orgIDs  map[string]string // org name -> id
	teamIDs map[string]string // "org/team" -> id
	minted  map[string]string // key name -> plain key, shown once
}

// Options tune a plan.
type Options struct {
	// Prune deletes what a managed section does not list. Admin-scope keys
	// are never revoked by a prune: one of them is likely the caller's own.
	Prune bool
	// Env looks up vendor keys; os.LookupEnv when nil.
	Env func(string) (string, bool)
}

// Empty reports whether applying the plan would change nothing.
func (p *Plan) Empty() bool { return len(p.steps) == 0 }

// Count returns the number of creates, updates and deletes.
func (p *Plan) Count() (create, update, del int) {
	for _, c := range p.Changes {
		switch c.Op {
		case Create:
			create++
		case Update:
			update++
		case Delete:
			del++
		}
	}
	return
}

// Write prints the plan for a person.
func (p *Plan) Write(w io.Writer) {
	for _, warn := range p.Warnings {
		fmt.Fprintf(w, "! %s\n", warn)
	}
	if p.Empty() {
		fmt.Fprintln(w, "No changes: Keystone matches the configuration.")
		return
	}
	mark := map[Op]string{Create: "+", Update: "~", Delete: "-"}
	for _, c := range p.Changes {
		fmt.Fprintf(w, "%s %s %s\n", mark[c.Op], c.Kind, c.Name)
		for _, d := range c.Details {
			fmt.Fprintf(w, "    %s\n", d)
		}
	}
	cr, up, de := p.Count()
	fmt.Fprintf(w, "Plan: %d to create, %d to change, %d to delete.\n", cr, up, de)
}

func (p *Plan) add(c Change, run func(context.Context, pb.AdminServiceClient, *applied) error) {
	p.Changes = append(p.Changes, c)
	p.steps = append(p.steps, step{change: c, run: run})
}

// Build compares the desired configuration with the live one. It fails when
// the two cannot be reconciled through the Admin API (a key whose scopes or
// team changed, a revoked key named again, a missing vendor key variable).
func Build(cfg *Config, live *State, opt Options) (*Plan, error) {
	if opt.Env == nil {
		opt.Env = os.LookupEnv
	}
	p := &Plan{}
	var conflicts []error
	conflict := func(format string, a ...any) { conflicts = append(conflicts, fmt.Errorf(format, a...)) }

	// Organizations and teams first: keys refer to them.
	var deletes []func()
	if cfg.Manages("organizations") {
		for _, o := range cfg.Organizations {
			o := o
			lo := live.orgByName(o.Name)
			if lo == nil {
				p.add(Change{Op: Create, Kind: "organization", Name: o.Name}, func(ctx context.Context, c pb.AdminServiceClient, a *applied) error {
					r, err := c.CreateOrganization(ctx, &pb.CreateOrganizationRequest{Name: o.Name})
					if err != nil {
						return err
					}
					a.orgIDs[o.Name] = r.GetOrganization().GetId()
					return nil
				})
			}
			for _, t := range o.Teams {
				t := t
				var lt *pb.Team
				if lo != nil {
					lt = live.team(lo.GetId(), t.Name)
				}
				name := o.Name + "/" + t.Name
				if lt == nil {
					p.add(Change{Op: Create, Kind: "team", Name: name, Details: teamDetails(nil, t)}, func(ctx context.Context, c pb.AdminServiceClient, a *applied) error {
						r, err := c.CreateTeam(ctx, &pb.CreateTeamRequest{OrgId: a.orgIDs[o.Name], Name: t.Name,
							MonthlyBudgetUsd: t.MonthlyBudgetUSD, AllowedModels: t.AllowedModels, AllowedMcp: t.AllowedMCP})
						if err != nil {
							return err
						}
						a.teamIDs[name] = r.GetTeam().GetId()
						return nil
					})
					continue
				}
				if d := teamDetails(lt, t); len(d) > 0 {
					p.add(Change{Op: Update, Kind: "team", Name: name, Details: d}, func(ctx context.Context, c pb.AdminServiceClient, _ *applied) error {
						_, err := c.UpdateTeam(ctx, &pb.UpdateTeamRequest{Id: lt.GetId(), Name: t.Name,
							MonthlyBudgetUsd: t.MonthlyBudgetUSD, AllowedModels: t.AllowedModels, AllowedMcp: t.AllowedMCP})
						return err
					})
				}
			}
		}
		if opt.Prune {
			for _, lt := range live.Teams {
				lt := lt
				lo := live.orgByID(lt.GetOrgId())
				if lo != nil && wantTeam(cfg, lo.GetName(), lt.GetName()) {
					continue
				}
				name := lo.GetName() + "/" + lt.GetName()
				deletes = append(deletes, func() {
					p.add(Change{Op: Delete, Kind: "team", Name: name}, func(ctx context.Context, c pb.AdminServiceClient, _ *applied) error {
						_, err := c.DeleteTeam(ctx, &pb.DeleteTeamRequest{Id: lt.GetId()})
						return err
					})
				})
			}
			for _, lo := range live.Orgs {
				lo := lo
				if slices.ContainsFunc(cfg.Organizations, func(o Organization) bool { return o.Name == lo.GetName() }) {
					continue
				}
				deletes = append(deletes, func() {
					p.add(Change{Op: Delete, Kind: "organization", Name: lo.GetName()}, func(ctx context.Context, c pb.AdminServiceClient, _ *applied) error {
						_, err := c.DeleteOrganization(ctx, &pb.DeleteOrganizationRequest{Id: lo.GetId()})
						return err
					})
				})
			}
		}
	}

	// Models before rules, which name them.
	liveModels := map[string]*pb.ModelSpec{}
	for _, m := range live.Config.GetModels() {
		liveModels[m.GetId()] = m
	}
	if cfg.Manages("models") {
		for _, m := range cfg.Models {
			m := m
			lm := liveModels[m.GetId()]
			if lm == nil {
				p.add(Change{Op: Create, Kind: "model", Name: m.GetId()}, upsertModel(m))
				continue
			}
			if d := fieldDiff(normModel(lm), normModel(m)); len(d) > 0 {
				p.add(Change{Op: Update, Kind: "model", Name: m.GetId(), Details: d}, upsertModel(m))
			}
		}
	}

	// MCP servers before their credentials.
	liveMCP := map[string]*pb.MCPServerSpec{}
	for _, m := range live.Config.GetMcpServers() {
		liveMCP[m.GetName()] = m
	}
	if cfg.Manages("mcp_servers") {
		for _, m := range cfg.MCPServers {
			m := m
			upsert := func(ctx context.Context, c pb.AdminServiceClient, _ *applied) error {
				_, err := c.UpsertMCPServer(ctx, &pb.UpsertMCPServerRequest{Server: m})
				return err
			}
			lm := liveMCP[m.GetName()]
			if lm == nil {
				p.add(Change{Op: Create, Kind: "mcp server", Name: m.GetName(), Details: []string{m.GetUrl()}}, upsert)
			} else if d := fieldDiff(normMCP(lm), normMCP(m)); len(d) > 0 {
				p.add(Change{Op: Update, Kind: "mcp server", Name: m.GetName(), Details: d}, upsert)
			}
		}
		if opt.Prune {
			for name := range liveMCP {
				name := name
				if slices.ContainsFunc(cfg.MCPServers, func(m *pb.MCPServerSpec) bool { return m.GetName() == name }) {
					continue
				}
				deletes = append(deletes, func() {
					p.add(Change{Op: Delete, Kind: "mcp server", Name: name, Details: []string{"its credential goes with it"}}, func(ctx context.Context, c pb.AdminServiceClient, _ *applied) error {
						_, err := c.DeleteMCPServer(ctx, &pb.DeleteMCPServerRequest{Name: name})
						return err
					})
				})
			}
		}
	}
	if cfg.Manages("mcp_credentials") {
		liveCred := map[string]*pb.MCPCredentialInfo{}
		for _, mc := range live.Config.GetMcpCredentials() {
			liveCred[mc.GetServer()] = mc
		}
		for _, mc := range cfg.MCPCredentials {
			mc := mc
			plain, ok := opt.Env(mc.ValueEnv)
			if !ok || plain == "" {
				conflict("mcp credential %s: environment variable %s is not set", mc.Server, mc.ValueEnv)
				continue
			}
			if liveMCP[mc.Server] == nil && !slices.ContainsFunc(cfg.MCPServers, func(m *pb.MCPServerSpec) bool { return m.GetName() == mc.Server }) {
				conflict("mcp credential %s: the server exists neither in Keystone nor in mcp_servers", mc.Server)
				continue
			}
			header := mc.Header
			if header == "" {
				header = "Authorization"
			}
			set := func(ctx context.Context, c pb.AdminServiceClient, _ *applied) error {
				_, err := c.SetMCPCredential(ctx, &pb.SetMCPCredentialRequest{Server: mc.Server, Header: header, PlainValue: plain})
				return err
			}
			lc := liveCred[mc.Server]
			switch {
			case lc == nil:
				p.add(Change{Op: Create, Kind: "mcp credential", Name: mc.Server, Details: []string{header + " from $" + mc.ValueEnv}}, set)
			case !strings.EqualFold(lc.GetHeader(), header):
				p.add(Change{Op: Update, Kind: "mcp credential", Name: mc.Server, Details: []string{"header " + lc.GetHeader() + " → " + header}}, set)
			case lc.GetFingerprint() == "":
				p.add(Change{Op: Update, Kind: "mcp credential", Name: mc.Server, Details: []string{"set again from $" + mc.ValueEnv + ": Keystone cannot tell whether it changed"}}, set)
			case lc.GetFingerprint() != crypto.Fingerprint(plain):
				p.add(Change{Op: Update, Kind: "mcp credential", Name: mc.Server, Details: []string{"changed in $" + mc.ValueEnv}}, set)
			}
		}
		if opt.Prune {
			for _, lc := range live.Config.GetMcpCredentials() {
				lc := lc
				if slices.ContainsFunc(cfg.MCPCredentials, func(mc MCPCredential) bool { return mc.Server == lc.GetServer() }) {
					continue
				}
				deletes = append(deletes, func() {
					p.add(Change{Op: Delete, Kind: "mcp credential", Name: lc.GetServer()}, func(ctx context.Context, c pb.AdminServiceClient, _ *applied) error {
						_, err := c.DeleteMCPCredential(ctx, &pb.DeleteMCPCredentialRequest{Server: lc.GetServer()})
						return err
					})
				})
			}
		}
	}

	// Keys: mint the missing ones, update policies, refuse what the API cannot change.
	if cfg.Manages("api_keys") {
		for _, k := range cfg.APIKeys {
			k := k
			lk := live.key(k.Name)
			scopes := k.Scopes
			if len(scopes) == 0 {
				scopes = []string{apikeys.ScopeGenerate}
			}
			if lk == nil {
				if k.Organization != "" && live.orgByName(k.Organization) == nil && !planned(cfg, k.Organization, "") {
					conflict("api key %s: organization %q exists neither in Keystone nor in organizations", k.Name, k.Organization)
					continue
				}
				if k.Team != "" && !teamExists(live, k.Organization, k.Team) && !planned(cfg, k.Organization, k.Team) {
					conflict("api key %s: team %q/%q exists neither in Keystone nor in organizations", k.Name, k.Organization, k.Team)
					continue
				}
				p.add(Change{Op: Create, Kind: "api key", Name: k.Name, Details: keyDetails(k, scopes)},
					func(ctx context.Context, c pb.AdminServiceClient, a *applied) error {
						req := &pb.MintAPIKeyRequest{Name: k.Name, Scopes: scopes, Policy: k.Policy}
						if k.Organization != "" {
							req.OrgId = a.orgIDs[k.Organization]
						}
						if k.Team != "" {
							req.TeamId = a.teamIDs[k.Organization+"/"+k.Team]
						}
						r, err := c.MintAPIKey(ctx, req)
						if err != nil {
							return err
						}
						a.minted[k.Name] = r.GetPlainKey()
						return nil
					})
				continue
			}
			if lk.GetDisabled() {
				conflict("api key %s is revoked and cannot be enabled again: give the key another name", k.Name)
				continue
			}
			if !sameSet(lk.GetScopes(), scopes) {
				conflict("api key %s has scopes %v, the file says %v: a key's scopes cannot change, mint a key under another name", k.Name, lk.GetScopes(), scopes)
			}
			if org, team := keyTenancy(live, lk); org != k.Organization || team != k.Team {
				conflict("api key %s belongs to %q/%q, the file says %q/%q: a key cannot move, mint a key under another name", k.Name, org, team, k.Organization, k.Team)
			}
			if d := policyDiff(lk.GetPolicy(), k.Policy); len(d) > 0 {
				p.add(Change{Op: Update, Kind: "api key policy", Name: k.Name, Details: d}, func(ctx context.Context, c pb.AdminServiceClient, _ *applied) error {
					pol := k.Policy
					if pol == nil {
						pol = &pb.APIKeyPolicy{}
					}
					_, err := c.SetAPIKeyPolicy(ctx, &pb.SetAPIKeyPolicyRequest{Name: k.Name, Policy: pol})
					return err
				})
			}
		}
		if opt.Prune {
			for _, lk := range live.Keys {
				lk := lk
				if lk.GetDisabled() || slices.ContainsFunc(cfg.APIKeys, func(k APIKey) bool { return k.Name == lk.GetName() }) {
					continue
				}
				if slices.Contains(lk.GetScopes(), apikeys.ScopeAdmin) || slices.Contains(lk.GetScopes(), apikeys.ScopeAdminRead) {
					p.Warnings = append(p.Warnings, fmt.Sprintf("admin key %s is not in the file; prune never revokes admin or admin:read keys, revoke it by hand if it should go", lk.GetName()))
					continue
				}
				deletes = append(deletes, func() {
					p.add(Change{Op: Delete, Kind: "api key", Name: lk.GetName(), Details: []string{"revoked: clients using it are refused at once"}},
						func(ctx context.Context, c pb.AdminServiceClient, _ *applied) error {
							_, err := c.RevokeAPIKey(ctx, &pb.RevokeAPIKeyRequest{Name: lk.GetName()})
							return err
						})
				})
			}
		}
	}

	// Vendor keys after the client keys they belong to.
	if cfg.Manages("vendor_keys") {
		liveVK := map[string]*pb.VendorKeyInfo{}
		for _, v := range live.Config.GetVendorKeys() {
			liveVK[v.GetApiKeyName()+"/"+v.GetVendor()] = v
		}
		for _, v := range cfg.VendorKeys {
			v := v
			id := v.APIKeyName + "/" + v.Vendor
			plain, ok := opt.Env(v.PlainKeyEnv)
			if !ok || plain == "" {
				conflict("vendor key %s: environment variable %s is not set", id, v.PlainKeyEnv)
				continue
			}
			set := func(ctx context.Context, c pb.AdminServiceClient, _ *applied) error {
				_, err := c.SetVendorKey(ctx, &pb.SetVendorKeyRequest{ApiKeyName: v.APIKeyName, Vendor: v.Vendor, PlainKey: plain})
				return err
			}
			lv := liveVK[id]
			switch {
			case lv == nil:
				p.add(Change{Op: Create, Kind: "vendor key", Name: id, Details: []string{"from $" + v.PlainKeyEnv}}, set)
			case lv.GetFingerprint() == "":
				p.add(Change{Op: Update, Kind: "vendor key", Name: id, Details: []string{"set again from $" + v.PlainKeyEnv + ": Keystone cannot tell whether it changed"}}, set)
			case lv.GetFingerprint() != crypto.Fingerprint(plain):
				p.add(Change{Op: Update, Kind: "vendor key", Name: id, Details: []string{"changed in $" + v.PlainKeyEnv}}, set)
			}
		}
		if opt.Prune {
			for _, lv := range live.Config.GetVendorKeys() {
				lv := lv
				if slices.ContainsFunc(cfg.VendorKeys, func(v VendorKey) bool { return v.APIKeyName == lv.GetApiKeyName() && v.Vendor == lv.GetVendor() }) {
					continue
				}
				deletes = append(deletes, func() {
					p.add(Change{Op: Delete, Kind: "vendor key", Name: lv.GetApiKeyName() + "/" + lv.GetVendor()}, func(ctx context.Context, c pb.AdminServiceClient, _ *applied) error {
						_, err := c.DeleteVendorKey(ctx, &pb.DeleteVendorKeyRequest{ApiKeyName: lv.GetApiKeyName(), Vendor: lv.GetVendor()})
						return err
					})
				})
			}
		}
	}

	// Rules are one atomic set.
	if cfg.Manages("rules") {
		if d := rulesDiff(live.Config.GetRules(), cfg.Rules); len(d) > 0 {
			rules := cfg.Rules
			p.add(Change{Op: Update, Kind: "rules", Name: fmt.Sprintf("(%d rules, replaced together)", len(rules)), Details: d},
				func(ctx context.Context, c pb.AdminServiceClient, _ *applied) error {
					_, err := c.ReplaceRules(ctx, &pb.ReplaceRulesRequest{Rules: rules})
					return err
				})
		}
	}

	if cfg.Manages("spend_limits") {
		liveSL := map[string]int64{}
		var liveSpecs []*pb.SpendLimitSpec
		for _, l := range live.Config.GetSpendLimits() {
			if l.GetSubjectType() != "team" {
				liveSL[l.GetSubjectType()+"/"+l.GetSubjectId()+"/"+l.GetPeriod()] = l.GetLimitNanousd()
				liveSpecs = append(liveSpecs, l)
			}
		}
		for _, l := range cfg.SpendLimits {
			l := l
			id := l.SubjectType + "/" + l.SubjectID + "/" + l.Period
			cur, ok := liveSL[id]
			set := func(ctx context.Context, c pb.AdminServiceClient, _ *applied) error {
				_, err := c.SetSpendLimit(ctx, &pb.SetSpendLimitRequest{SpendLimit: &pb.SpendLimitSpec{
					SubjectType: l.SubjectType, SubjectId: l.SubjectID, Period: l.Period, LimitNanousd: l.Nano()}})
				return err
			}
			if !ok {
				p.add(Change{Op: Create, Kind: "spend limit", Name: id, Details: []string{usd(l.Nano())}}, set)
			} else if cur != l.Nano() {
				p.add(Change{Op: Update, Kind: "spend limit", Name: id, Details: []string{usd(cur) + " → " + usd(l.Nano())}}, set)
			}
		}
		if opt.Prune {
			for _, l := range liveSpecs {
				l := l
				if slices.ContainsFunc(cfg.SpendLimits, func(w SpendLimit) bool {
					return w.SubjectType == l.GetSubjectType() && w.SubjectID == l.GetSubjectId() && w.Period == l.GetPeriod()
				}) {
					continue
				}
				id := l.GetSubjectType() + "/" + l.GetSubjectId() + "/" + l.GetPeriod()
				deletes = append(deletes, func() {
					p.add(Change{Op: Delete, Kind: "spend limit", Name: id}, func(ctx context.Context, c pb.AdminServiceClient, _ *applied) error {
						_, err := c.DeleteSpendLimit(ctx, &pb.DeleteSpendLimitRequest{SubjectType: l.GetSubjectType(), SubjectId: l.GetSubjectId(), Period: l.GetPeriod()})
						return err
					})
				})
			}
		}
	}

	if cfg.Manages("user_rate_limits") {
		liveUR := map[string]*pb.UserRateLimitSpec{}
		for _, u := range live.Config.GetUserRateLimits() {
			liveUR[u.GetUserId()+"/"+u.GetModelId()] = u
		}
		for _, u := range cfg.UserRateLimits {
			u := u
			id := u.GetUserId() + "/" + u.GetModelId()
			set := func(ctx context.Context, c pb.AdminServiceClient, _ *applied) error {
				_, err := c.SetUserRateLimit(ctx, &pb.SetUserRateLimitRequest{RateLimit: u})
				return err
			}
			if lu := liveUR[id]; lu == nil {
				p.add(Change{Op: Create, Kind: "user rate limit", Name: id}, set)
			} else if d := fieldDiff(lu, u); len(d) > 0 {
				p.add(Change{Op: Update, Kind: "user rate limit", Name: id, Details: d}, set)
			}
		}
		if opt.Prune {
			for _, lu := range live.Config.GetUserRateLimits() {
				lu := lu
				id := lu.GetUserId() + "/" + lu.GetModelId()
				if slices.ContainsFunc(cfg.UserRateLimits, func(u *pb.UserRateLimitSpec) bool {
					return u.GetUserId() == lu.GetUserId() && u.GetModelId() == lu.GetModelId()
				}) {
					continue
				}
				deletes = append(deletes, func() {
					p.add(Change{Op: Delete, Kind: "user rate limit", Name: id}, func(ctx context.Context, c pb.AdminServiceClient, _ *applied) error {
						_, err := c.DeleteUserRateLimit(ctx, &pb.DeleteUserRateLimitRequest{UserId: lu.GetUserId(), ModelId: lu.GetModelId()})
						return err
					})
				})
			}
		}
	}

	if cfg.Manages("node_schedules") {
		if live.NodeSchedulesErr != nil {
			conflict("node_schedules: %v", live.NodeSchedulesErr)
		}
		liveNS := map[string]string{}
		for _, ns := range live.NodeSchedules {
			if ns.GetVersion() > 0 {
				liveNS[ns.GetNodeId()] = ns.GetText()
			}
		}
		for _, ns := range cfg.NodeSchedules {
			ns := ns
			set := func(ctx context.Context, c pb.AdminServiceClient, _ *applied) error {
				_, err := c.SetNodeSchedule(ctx, &pb.SetNodeScheduleRequest{NodeId: ns.NodeID, Text: ns.Schedule})
				return err
			}
			cur, ok := liveNS[ns.NodeID]
			switch {
			case !ok:
				p.add(Change{Op: Create, Kind: "node schedule", Name: ns.NodeID}, set)
			case strings.TrimSpace(cur) != strings.TrimSpace(ns.Schedule):
				p.add(Change{Op: Update, Kind: "node schedule", Name: ns.NodeID, Details: []string{"rules changed"}}, set)
			}
		}
		if opt.Prune {
			for id := range liveNS {
				id := id
				if slices.ContainsFunc(cfg.NodeSchedules, func(ns NodeSchedule) bool { return ns.NodeID == id }) {
					continue
				}
				deletes = append(deletes, func() {
					p.add(Change{Op: Delete, Kind: "node schedule", Name: id}, func(ctx context.Context, c pb.AdminServiceClient, _ *applied) error {
						_, err := c.DeleteNodeSchedule(ctx, &pb.DeleteNodeScheduleRequest{NodeId: id})
						return err
					})
				})
			}
		}
	}

	if cfg.Manages("models") && opt.Prune {
		var gone []string
		for id := range liveModels {
			if !slices.ContainsFunc(cfg.Models, func(m *pb.ModelSpec) bool { return m.GetId() == id }) {
				gone = append(gone, id)
			}
		}
		sort.Strings(gone)
		for _, id := range gone {
			id := id
			deletes = append(deletes, func() {
				p.add(Change{Op: Delete, Kind: "model", Name: id}, func(ctx context.Context, c pb.AdminServiceClient, _ *applied) error {
					_, err := c.DeleteModel(ctx, &pb.DeleteModelRequest{Id: id})
					return err
				})
			})
		}
	}
	for _, d := range deletes {
		d()
	}

	if len(conflicts) > 0 {
		return nil, joinConflicts(conflicts)
	}
	sortDeletes(p)
	return p, nil
}

// sortDeletes runs deletes after everything else, each kind after what
// refers to it: limits and keys, then models (the new rules no longer name
// them), then teams, then organizations.
func sortDeletes(p *Plan) {
	order := map[string]int{"user rate limit": 1, "spend limit": 1, "vendor key": 2, "mcp credential": 2, "api key": 3, "model": 4, "mcp server": 4, "team": 5, "organization": 6}
	rank := func(s step) int {
		if s.change.Op != Delete {
			return 0
		}
		return order[s.change.Kind]
	}
	idx := make([]int, len(p.steps))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return rank(p.steps[idx[a]]) < rank(p.steps[idx[b]]) })
	steps := make([]step, len(idx))
	changes := make([]Change, len(idx))
	for i, j := range idx {
		steps[i], changes[i] = p.steps[j], p.Changes[j]
	}
	p.steps, p.Changes = steps, changes
}

func joinConflicts(errs []error) error {
	lines := make([]string, len(errs))
	for i, e := range errs {
		lines[i] = "  " + e.Error()
	}
	return errors.New("the configuration cannot be applied as is:\n" + strings.Join(lines, "\n"))
}

// Result is what an apply produced that only it can show.
type Result struct {
	// Minted holds new client keys by name. They exist nowhere else: show them once.
	Minted map[string]string
}

// Apply runs the plan's steps in order and stops at the first error. The
// steps before it stay applied; planning again shows what is left.
func Apply(ctx context.Context, c pb.AdminServiceClient, live *State, p *Plan, progress io.Writer) (*Result, error) {
	a := &applied{orgIDs: map[string]string{}, teamIDs: map[string]string{}, minted: map[string]string{}}
	for _, o := range live.Orgs {
		a.orgIDs[o.GetName()] = o.GetId()
	}
	for _, t := range live.Teams {
		if o := live.orgByID(t.GetOrgId()); o != nil {
			a.teamIDs[o.GetName()+"/"+t.GetName()] = t.GetId()
		}
	}
	for i, s := range p.steps {
		if err := s.run(ctx, c, a); err != nil {
			return &Result{Minted: a.minted}, fmt.Errorf("step %d of %d, %s %s %s: %w", i+1, len(p.steps), s.change.Op, s.change.Kind, s.change.Name, err)
		}
		if progress != nil {
			fmt.Fprintf(progress, "done: %s %s %s\n", s.change.Op, s.change.Kind, s.change.Name)
		}
	}
	return &Result{Minted: a.minted}, nil
}

func upsertModel(m *pb.ModelSpec) func(context.Context, pb.AdminServiceClient, *applied) error {
	return func(ctx context.Context, c pb.AdminServiceClient, _ *applied) error {
		_, err := c.UpsertModel(ctx, &pb.UpsertModelRequest{Model: m})
		return err
	}
}

// planned reports whether the file's organizations create the organization
// (team empty) or the team.
func planned(cfg *Config, org, team string) bool {
	for _, o := range cfg.Organizations {
		if o.Name == org {
			return team == "" || slices.ContainsFunc(o.Teams, func(t Team) bool { return t.Name == team })
		}
	}
	return false
}

func teamExists(live *State, org, team string) bool {
	o := live.orgByName(org)
	return o != nil && live.team(o.GetId(), team) != nil
}

func wantTeam(cfg *Config, org, team string) bool {
	for _, o := range cfg.Organizations {
		if o.Name == org {
			return slices.ContainsFunc(o.Teams, func(t Team) bool { return t.Name == team })
		}
	}
	return false
}

func keyTenancy(live *State, k *pb.APIKeyInfo) (org, team string) {
	if o := live.orgByID(k.GetOrgId()); o != nil {
		org = o.GetName()
	}
	if t := live.teamByID(k.GetTeamId()); t != nil {
		team = t.GetName()
	}
	return org, team
}

func keyDetails(k APIKey, scopes []string) []string {
	d := []string{"scopes " + strings.Join(scopes, ", "), "the new key is shown once, after apply"}
	if k.Team != "" {
		d = append(d, "team "+k.Organization+"/"+k.Team)
	} else if k.Organization != "" {
		d = append(d, "organization "+k.Organization)
	}
	return d
}

func teamDetails(live *pb.Team, t Team) []string {
	var d []string
	if live == nil {
		if t.MonthlyBudgetUSD > 0 {
			d = append(d, fmt.Sprintf("monthly budget $%g", t.MonthlyBudgetUSD))
		}
		if len(t.AllowedModels) > 0 {
			d = append(d, "allowed models "+strings.Join(t.AllowedModels, ", "))
		}
		if len(t.AllowedMCP) > 0 {
			d = append(d, "allowed mcp "+strings.Join(t.AllowedMCP, ", "))
		}
		return d
	}
	if live.GetMonthlyBudgetUsd() != t.MonthlyBudgetUSD {
		d = append(d, fmt.Sprintf("monthly budget $%g → $%g", live.GetMonthlyBudgetUsd(), t.MonthlyBudgetUSD))
	}
	if !sameSet(live.GetAllowedModels(), t.AllowedModels) {
		d = append(d, fmt.Sprintf("allowed models %v → %v", live.GetAllowedModels(), t.AllowedModels))
	}
	if !sameSet(live.GetAllowedMcp(), t.AllowedMCP) {
		d = append(d, fmt.Sprintf("allowed mcp %v → %v", live.GetAllowedMcp(), t.AllowedMCP))
	}
	return d
}

func policyDiff(live, want *pb.APIKeyPolicy) []string {
	var d []string
	if !sameSet(live.GetAllowedModels(), want.GetAllowedModels()) {
		d = append(d, fmt.Sprintf("allowed models %v → %v", live.GetAllowedModels(), want.GetAllowedModels()))
	}
	if live.GetRpmLimit() != want.GetRpmLimit() {
		d = append(d, fmt.Sprintf("rpm_limit %d → %d", live.GetRpmLimit(), want.GetRpmLimit()))
	}
	if live.GetTpmLimit() != want.GetTpmLimit() {
		d = append(d, fmt.Sprintf("tpm_limit %d → %d", live.GetTpmLimit(), want.GetTpmLimit()))
	}
	if !sameSet(live.GetAllowedMcp(), want.GetAllowedMcp()) {
		d = append(d, fmt.Sprintf("allowed mcp %v → %v", live.GetAllowedMcp(), want.GetAllowedMcp()))
	}
	return d
}

// rulesDiff names the rules added, removed and changed, matched by name.
func rulesDiff(live, want []*pb.RuleSpec) []string {
	byName := func(rs []*pb.RuleSpec) map[string]*pb.RuleSpec {
		m := map[string]*pb.RuleSpec{}
		for _, r := range rs {
			m[r.GetName()] = r
		}
		return m
	}
	l, w := byName(live), byName(want)
	var d []string
	for _, r := range want {
		lr := l[r.GetName()]
		if lr == nil {
			d = append(d, "+ rule "+r.GetName())
			continue
		}
		for _, f := range fieldDiff(lr, r) {
			d = append(d, "~ rule "+r.GetName()+": "+f)
		}
	}
	for _, r := range live {
		if w[r.GetName()] == nil {
			d = append(d, "- rule "+r.GetName())
		}
	}
	return d
}

// normModel orders the set-like lists, so their order is not a difference.
func normModel(m *pb.ModelSpec) *pb.ModelSpec {
	n := proto.Clone(m).(*pb.ModelSpec)
	slices.Sort(n.Capabilities)
	slices.Sort(n.Efforts)
	return n
}

// normMCP spells out the default transport, so leaving it out is no difference.
func normMCP(m *pb.MCPServerSpec) *pb.MCPServerSpec {
	n := proto.Clone(m).(*pb.MCPServerSpec)
	if n.Transport == "" {
		n.Transport = "streamable_http"
	}
	return n
}

// fieldDiff lists the fields of two messages of one type that differ, as
// "field old → new".
func fieldDiff(a, b proto.Message) []string {
	ra, rb := a.ProtoReflect(), b.ProtoReflect()
	fields := ra.Descriptor().Fields()
	var d []string
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		va, vb := ra.Get(fd), rb.Get(fd)
		if !valueEqual(fd, va, vb) {
			d = append(d, fmt.Sprintf("%s %s → %s", fd.Name(), show(fd, va), show(fd, vb)))
		}
	}
	return d
}

func valueEqual(fd protoreflect.FieldDescriptor, a, b protoreflect.Value) bool {
	return show(fd, a) == show(fd, b)
}

func show(fd protoreflect.FieldDescriptor, v protoreflect.Value) string {
	switch {
	case fd.IsList():
		l := v.List()
		parts := make([]string, l.Len())
		for i := range parts {
			parts[i] = scalar(fd, l.Get(i))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case fd.IsMap():
		var parts []string
		v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
			parts = append(parts, k.String()+"="+mv.String())
			return true
		})
		sort.Strings(parts)
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return scalar(fd, v)
}

func scalar(fd protoreflect.FieldDescriptor, v protoreflect.Value) string {
	if fd.Kind() == protoreflect.EnumKind {
		if ev := fd.Enum().Values().ByNumber(v.Enum()); ev != nil {
			return string(ev.Name())
		}
	}
	if fd.Kind() == protoreflect.StringKind {
		return fmt.Sprintf("%q", v.String())
	}
	return v.String()
}

func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

func usd(nano int64) string { return fmt.Sprintf("$%.9g", float64(nano)/1e9) }
