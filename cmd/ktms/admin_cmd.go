package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

// call runs fn against the Admin API within --timeout.
func (a *app) call(fn func(context.Context, pb.AdminServiceClient) error) error {
	return a.callCtx(context.Background(), fn)
}

// callCtx is call under a parent context, such as an MCP request's. A long
// running server keeps one connection in a.admin instead of dialing per call.
func (a *app) callCtx(ctx context.Context, fn func(context.Context, pb.AdminServiceClient) error) error {
	c := a.admin
	if c == nil {
		var closeConn func()
		var err error
		if c, closeConn, err = a.t.admin(); err != nil {
			return err
		}
		defer closeConn()
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	return fn(ctx, c)
}

// mintRequest names the key's organization and team by id, as the API wants.
func mintRequest(ctx context.Context, c pb.AdminServiceClient, name string, scopes []string, org, team string, pol *pb.APIKeyPolicy) (*pb.MintAPIKeyRequest, error) {
	req := &pb.MintAPIKeyRequest{Name: name, Scopes: scopes, Policy: pol}
	if org == "" && team == "" {
		return req, nil
	}
	t, err := loadTenants(ctx, c)
	if err != nil {
		return nil, err
	}
	if org != "" {
		if req.OrgId, err = t.org(org); err != nil {
			return nil, err
		}
	}
	if team != "" {
		if req.TeamId, err = t.team(team); err != nil {
			return nil, err
		}
	}
	return req, nil
}

// tenants maps organization and team ids to names and back; teams go by
// "org/team".
type tenants struct {
	orgName, teamName map[string]string
	orgID, teamID     map[string]string
	teams             []*pb.Team
}

func loadTenants(ctx context.Context, c pb.AdminServiceClient) (*tenants, error) {
	orgs, err := c.ListOrganizations(ctx, &pb.ListOrganizationsRequest{})
	if err != nil {
		return nil, err
	}
	teams, err := c.ListTeams(ctx, &pb.ListTeamsRequest{})
	if err != nil {
		return nil, err
	}
	t := &tenants{orgName: map[string]string{}, teamName: map[string]string{}, orgID: map[string]string{}, teamID: map[string]string{},
		teams: teams.GetTeams()}
	for _, o := range orgs.GetOrganizations() {
		t.orgName[o.GetId()], t.orgID[o.GetName()] = o.GetName(), o.GetId()
	}
	for _, tm := range teams.GetTeams() {
		full := t.orgName[tm.GetOrgId()] + "/" + tm.GetName()
		t.teamName[tm.GetId()], t.teamID[full] = full, tm.GetId()
	}
	return t, nil
}

func (t *tenants) org(name string) (string, error) {
	if id, ok := t.orgID[name]; ok {
		return id, nil
	}
	if _, ok := t.orgName[name]; ok {
		return name, nil
	}
	return "", fmt.Errorf("no organization %q", name)
}

func (t *tenants) team(ref string) (string, error) {
	if id, ok := t.teamID[ref]; ok {
		return id, nil
	}
	if _, ok := t.teamName[ref]; ok {
		return ref, nil
	}
	return "", fmt.Errorf("no team %q (name it org/team)", ref)
}

func (a *app) modelsCmd() *cobra.Command {
	var format string
	ls := &cobra.Command{
		Use: "list", Short: "List models", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				cfg, err := c.GetConfig(ctx, &pb.GetConfigRequest{})
				if err != nil {
					return err
				}
				ms := cfg.GetModels()
				sort.Slice(ms, func(i, j int) bool { return ms[i].GetId() < ms[j].GetId() })
				if !p.table() {
					return p.data(messages(ms))
				}
				var rows [][]string
				for _, m := range ms {
					efforts := make([]string, len(m.GetEfforts()))
					for i, e := range m.GetEfforts() {
						efforts[i] = strings.ToLower(strings.TrimPrefix(e.String(), "EFFORT_"))
					}
					rows = append(rows, []string{m.GetId(), m.GetVendor(), list(efforts),
						fmt.Sprintf("%g/%g", m.GetPriceInPerMtok(), m.GetPriceOutPerMtok()), yesNo(m.GetEnabled())})
				}
				return p.rows([]string{"ID", "VENDOR", "EFFORTS", "USD/MTOK IN/OUT", "ENABLED"}, rows)
			})
		},
	}
	outputFlag(ls, &format)
	cmd := &cobra.Command{Use: "models", Short: "Models in the registry"}
	cmd.AddCommand(ls)
	return cmd
}

func (a *app) rulesCmd() *cobra.Command {
	var format string
	ls := &cobra.Command{
		Use: "list", Short: "List routing rules, first match first", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				cfg, err := c.GetConfig(ctx, &pb.GetConfigRequest{})
				if err != nil {
					return err
				}
				rs := cfg.GetRules()
				sort.Slice(rs, func(i, j int) bool { return rs[i].GetPriority() > rs[j].GetPriority() })
				if !p.table() {
					return p.data(messages(rs))
				}
				var rows [][]string
				for _, r := range rs {
					effort := "any"
					if r.GetEffort() != pb.Effort_EFFORT_UNSPECIFIED {
						effort = strings.ToLower(strings.TrimPrefix(r.GetEffort().String(), "EFFORT_"))
					}
					attrs := make([]string, 0, len(r.GetAttributes()))
					for k, v := range r.GetAttributes() {
						attrs = append(attrs, k+"="+v)
					}
					sort.Strings(attrs)
					rows = append(rows, []string{strconv.Itoa(int(r.GetPriority())), r.GetName(), effort, list(attrs),
						strings.Join(r.GetUse(), " → "), yesNo(r.GetEnabled())})
				}
				return p.rows([]string{"PRIORITY", "NAME", "EFFORT", "ATTRIBUTES", "USE", "ENABLED"}, rows)
			})
		},
	}
	outputFlag(ls, &format)
	cmd := &cobra.Command{Use: "rules", Short: "Routing rules"}
	cmd.AddCommand(ls)
	return cmd
}

func (a *app) keysCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "keys", Short: "Client API keys"}

	var format string
	var all bool
	ls := &cobra.Command{
		Use: "list", Short: "List client keys (never the keys themselves)", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				keys, err := c.ListAPIKeys(ctx, &pb.ListAPIKeysRequest{})
				if err != nil {
					return err
				}
				var ks []*pb.APIKeyInfo
				for _, k := range keys.GetKeys() {
					if all || !k.GetDisabled() {
						ks = append(ks, k)
					}
				}
				if !p.table() {
					return p.data(messages(ks))
				}
				t, err := loadTenants(ctx, c)
				if err != nil {
					return err
				}
				var rows [][]string
				for _, k := range ks {
					tenant := orDash(t.teamName[k.GetTeamId()])
					if k.GetTeamId() == "" && k.GetOrgId() != "" {
						tenant = t.orgName[k.GetOrgId()]
					}
					pol := k.GetPolicy()
					rows = append(rows, []string{k.GetName(), list(k.GetScopes()), tenant, list(pol.GetAllowedModels()),
						limit(int64(pol.GetRpmLimit())), limit(pol.GetTpmLimit()), list(pol.GetAllowedMcp()), orDash(pol.GetDataClassification()),
						when(k.GetCreatedAt()), yesNo(k.GetDisabled())})
				}
				return p.rows([]string{"NAME", "SCOPES", "TENANT", "MODELS", "RPM", "TPM", "MCP", "DATA", "CREATED", "REVOKED"}, rows)
			})
		},
	}
	outputFlag(ls, &format)
	ls.Flags().BoolVar(&all, "all", false, "include revoked keys")

	var scopes, models, mcpAllow []string
	var org, team, dataClass string
	var rpm int32
	var tpm int64
	mint := &cobra.Command{
		Use:   "mint NAME",
		Short: "Mint a client key; it is printed once",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				req, err := mintRequest(ctx, c, args[0], scopes, org, team, policy(models, rpm, tpm, mcpAllow, dataClass))
				if err != nil {
					return err
				}
				res, err := c.MintAPIKey(ctx, req)
				if err != nil {
					return err
				}
				if !p.table() {
					return p.data(map[string]any{"name": args[0], "plain_key": res.GetPlainKey()})
				}
				fmt.Fprintf(a.stderr, "Key %s minted. It is shown only now; store it in your secret manager:\n", args[0])
				fmt.Fprintln(a.stdout, res.GetPlainKey())
				return nil
			})
		},
	}
	outputFlag(mint, &format)
	mint.Flags().StringSliceVar(&scopes, "scopes", []string{"generate"}, "generate, mcp, admin or admin:read")
	mint.Flags().StringVar(&org, "org", "", "organization name")
	mint.Flags().StringVar(&team, "team", "", "team as org/team")
	policyFlags(mint, &models, &rpm, &tpm, &mcpAllow, &dataClass)

	var yes bool
	revoke := &cobra.Command{
		Use: "revoke NAME", Short: "Revoke a client key; its clients are refused at once", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if !yes {
				if err := a.confirm("Revoke key " + args[0] + "?"); err != nil {
					return err
				}
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				if _, err := c.RevokeAPIKey(ctx, &pb.RevokeAPIKeyRequest{Name: args[0]}); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Revoked %s.\n", args[0])
				return nil
			})
		},
	}
	revoke.Flags().BoolVar(&yes, "yes", false, "revoke without asking")

	var pModels, pMCP []string
	var pData string
	var pRPM int32
	var pTPM int64
	set := &cobra.Command{
		Use:   "set NAME",
		Short: "Replace a key's policy; flags left out mean no limit",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				if _, err := c.SetAPIKeyPolicy(ctx, &pb.SetAPIKeyPolicyRequest{Name: args[0], Policy: policy(pModels, pRPM, pTPM, pMCP, pData)}); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Policy of %s replaced.\n", args[0])
				return nil
			})
		},
	}
	policyFlags(set, &pModels, &pRPM, &pTPM, &pMCP, &pData)
	pol := &cobra.Command{Use: "policy", Short: "A key's own limits"}
	pol.AddCommand(set)

	cmd.AddCommand(ls, mint, revoke, pol)
	return cmd
}

func policyFlags(cmd *cobra.Command, models *[]string, rpm *int32, tpm *int64, mcpAllow *[]string, dataClass *string) {
	cmd.Flags().StringSliceVar(models, "allowed-models", nil, "model ids the key may call (default any)")
	cmd.Flags().StringSliceVar(mcpAllow, "allowed-mcp", nil, "MCP servers (server) and tools (server/tool) the key may use (default any)")
	cmd.Flags().Int32Var(rpm, "rpm", 0, "calls per minute (default unlimited)")
	cmd.Flags().Int64Var(tpm, "tpm", 0, "tokens per minute (default unlimited)")
	cmd.Flags().StringVar(dataClass, "data-classification", "", "restricted keeps every call of the key on self-hosted nodes")
}

func policy(models []string, rpm int32, tpm int64, mcpAllow []string, dataClass string) *pb.APIKeyPolicy {
	if len(models) == 0 && rpm == 0 && tpm == 0 && len(mcpAllow) == 0 && dataClass == "" {
		return nil
	}
	return &pb.APIKeyPolicy{AllowedModels: models, RpmLimit: rpm, TpmLimit: tpm, AllowedMcp: mcpAllow, DataClassification: dataClass}
}

func limit(n int64) string {
	if n == 0 {
		return "-"
	}
	return strconv.FormatInt(n, 10)
}

func (a *app) orgsCmd() *cobra.Command {
	var format string
	ls := &cobra.Command{
		Use: "list", Short: "List organizations", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				res, err := c.ListOrganizations(ctx, &pb.ListOrganizationsRequest{})
				if err != nil {
					return err
				}
				if !p.table() {
					return p.data(messages(res.GetOrganizations()))
				}
				var rows [][]string
				for _, o := range res.GetOrganizations() {
					rows = append(rows, []string{o.GetName(), o.GetId(), when(o.GetCreatedAt())})
				}
				return p.rows([]string{"NAME", "ID", "CREATED"}, rows)
			})
		},
	}
	outputFlag(ls, &format)
	cmd := &cobra.Command{Use: "orgs", Short: "Organizations"}
	cmd.AddCommand(ls)
	return cmd
}

func (a *app) teamsCmd() *cobra.Command {
	var format, org string
	ls := &cobra.Command{
		Use: "list [--org NAME]", Short: "List teams", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				t, err := loadTenants(ctx, c)
				if err != nil {
					return err
				}
				orgID := ""
				if org != "" {
					if orgID, err = t.org(org); err != nil {
						return err
					}
				}
				var teams []*pb.Team
				for _, tm := range t.teams {
					if orgID == "" || tm.GetOrgId() == orgID {
						teams = append(teams, tm)
					}
				}
				if !p.table() {
					return p.data(messages(teams))
				}
				var rows [][]string
				for _, tm := range teams {
					budget := "-"
					if tm.GetMonthlyBudgetUsd() > 0 {
						budget = fmt.Sprintf("$%g", tm.GetMonthlyBudgetUsd())
					}
					rows = append(rows, []string{t.teamName[tm.GetId()], tm.GetId(), budget, list(tm.GetAllowedModels())})
				}
				return p.rows([]string{"TEAM", "ID", "MONTHLY BUDGET", "MODELS"}, rows)
			})
		},
	}
	outputFlag(ls, &format)
	ls.Flags().StringVar(&org, "org", "", "only this organization's teams")
	cmd := &cobra.Command{Use: "teams", Short: "Teams"}
	cmd.AddCommand(ls)
	return cmd
}

func (a *app) spendCmd() *cobra.Command {
	var format string
	get := &cobra.Command{
		Use:       "get key|user|team ID",
		Short:     "Spend against the caps of a key (by name), a user or a team (org/team)",
		Args:      cobra.ExactArgs(2),
		ValidArgs: []string{"key", "user", "team"},
		RunE: func(_ *cobra.Command, args []string) error {
			subject, id := args[0], args[1]
			if subject != "key" && subject != "user" && subject != "team" {
				return usageError{fmt.Errorf("subject %q: want key, user or team", subject)}
			}
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				if subject == "team" {
					t, err := loadTenants(ctx, c)
					if err != nil {
						return err
					}
					if id, err = t.team(id); err != nil {
						return err
					}
				}
				res, err := c.GetSpend(ctx, &pb.GetSpendRequest{SubjectType: subject, SubjectId: id})
				if err != nil {
					return err
				}
				if !p.table() {
					return p.data(messages(res.GetWindows()))
				}
				var rows [][]string
				for _, w := range res.GetWindows() {
					capped, spent, left := "-", "-", "-"
					if w.GetLimitNanousd() > 0 {
						capped = usd(w.GetLimitNanousd())
					}
					if w.GetMetered() {
						spent, left = usd(w.GetSpentNanousd()), usd(w.GetRemainingNanousd())
					}
					rows = append(rows, []string{w.GetPeriod(), spent, capped, left, when(w.GetResetsAt())})
				}
				if err := p.rows([]string{"PERIOD", "SPENT", "LIMIT", "REMAINING", "RESETS"}, rows); err != nil {
					return err
				}
				for _, w := range res.GetWindows() {
					if w.GetLimitNanousd() > 0 && !w.GetMetered() {
						fmt.Fprintln(a.stderr, "Spend is not metered: Keystone has no Redis.")
						break
					}
				}
				return nil
			})
		},
	}
	outputFlag(get, &format)
	cmd := &cobra.Command{Use: "spend", Short: "Spend against the caps", Long: "Spend is counted only under a cap (ktms limits set, a team's monthly budget)."}
	cmd.AddCommand(get)
	return cmd
}

func (a *app) limitsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "limits", Short: "Spend limits of keys and users"}
	var format string
	ls := &cobra.Command{
		Use: "list", Short: "List spend limits", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				res, err := c.ListSpendLimits(ctx, &pb.ListSpendLimitsRequest{})
				if err != nil {
					return err
				}
				if !p.table() {
					return p.data(messages(res.GetSpendLimits()))
				}
				var rows [][]string
				for _, l := range res.GetSpendLimits() {
					rows = append(rows, []string{l.GetSubjectType(), l.GetSubjectId(), l.GetPeriod(), usd(l.GetLimitNanousd())})
				}
				return p.rows([]string{"SUBJECT", "ID", "PERIOD", "LIMIT"}, rows)
			})
		},
	}
	outputFlag(ls, &format)

	subjectArgs := func(args []string) error {
		if args[0] != "key" && args[0] != "user" {
			return usageError{fmt.Errorf("subject %q: want key or user (a team's cap is its monthly budget)", args[0])}
		}
		if args[2] != "day" && args[2] != "month" {
			return usageError{fmt.Errorf("period %q: want day or month", args[2])}
		}
		return nil
	}
	set := &cobra.Command{
		Use:   "set key|user ID|* day|month USD",
		Short: "Cap a key's or user's spend per UTC day or month; * sets the default",
		Args:  cobra.ExactArgs(4),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := subjectArgs(args); err != nil {
				return err
			}
			dollars, err := strconv.ParseFloat(args[3], 64)
			if err != nil || !(dollars > 0 && dollars < math.MaxInt64/1e9) {
				return usageError{fmt.Errorf("limit %q: want a positive amount in USD", args[3])}
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				_, err := c.SetSpendLimit(ctx, &pb.SetSpendLimitRequest{SpendLimit: &pb.SpendLimitSpec{
					SubjectType: args[0], SubjectId: args[1], Period: args[2], LimitNanousd: int64(math.Round(dollars * 1e9)),
				}})
				if err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Limit set: %s %s, $%g per %s.\n", args[0], args[1], dollars, args[2])
				return nil
			})
		},
	}
	del := &cobra.Command{
		Use: "delete key|user ID|* day|month", Short: "Remove a spend limit", Args: cobra.ExactArgs(3),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := subjectArgs(args); err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				if _, err := c.DeleteSpendLimit(ctx, &pb.DeleteSpendLimitRequest{SubjectType: args[0], SubjectId: args[1], Period: args[2]}); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Limit removed: %s %s per %s.\n", args[0], args[1], args[2])
				return nil
			})
		},
	}
	cmd.AddCommand(ls, set, del)
	return cmd
}

func (a *app) nodesCmd() *cobra.Command {
	var format string
	ls := &cobra.Command{
		Use: "list", Short: "List registered inference nodes", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				res, err := c.ListNodes(ctx, &pb.ListNodesRequest{})
				if err != nil {
					return err
				}
				if !p.table() {
					return p.data(messages(res.GetNodes()))
				}
				var rows [][]string
				for _, n := range res.GetNodes() {
					rows = append(rows, []string{n.GetNodeId(), orDash(n.GetName()), n.GetState(), orDash(n.GetEngine()), list(n.GetModels()),
						fmt.Sprintf("%d/%d", n.GetActiveRequests(), n.GetMaxConcurrent()), fmt.Sprintf("%.2f", n.GetEffectiveWeight()), when(n.GetLastHeartbeat())})
				}
				return p.rows([]string{"NODE", "NAME", "STATE", "ENGINE", "MODELS", "ACTIVE", "WEIGHT", "HEARTBEAT"}, rows)
			})
		},
	}
	outputFlag(ls, &format)
	drain := &cobra.Command{
		Use: "drain NODE_ID", Short: "Stop sending new requests to a node; running ones finish", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				if _, err := c.DrainNode(ctx, &pb.DrainNodeRequest{NodeId: args[0]}); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Draining %s.\n", args[0])
				return nil
			})
		},
	}
	cmd := &cobra.Command{Use: "nodes", Short: "Inference nodes"}
	cmd.AddCommand(ls, drain)
	return cmd
}

func (a *app) nodeKeysCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "node-keys", Short: "Keys inference nodes register with"}
	var format string
	ls := &cobra.Command{
		Use: "list", Short: "List node keys (never the keys themselves)", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				res, err := c.ListNodeKeys(ctx, &pb.ListNodeKeysRequest{})
				if err != nil {
					return err
				}
				if !p.table() {
					return p.data(messages(res.GetKeys()))
				}
				var rows [][]string
				for _, k := range res.GetKeys() {
					rows = append(rows, []string{strconv.FormatInt(k.GetId(), 10), k.GetName(), orDash(k.GetNodeId()), list(k.GetAllowedCidrs()),
						list(k.GetAllowedModels()), when(k.GetCreatedAt()), yesNo(k.GetDisabled())})
				}
				return p.rows([]string{"ID", "NAME", "NODE", "CIDRS", "MODELS", "CREATED", "REVOKED"}, rows)
			})
		},
	}
	outputFlag(ls, &format)

	req := &pb.MintNodeKeyRequest{}
	mint := &cobra.Command{
		Use: "mint NAME --node-id ID", Short: "Mint a node key; it is printed once", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			req.Name = args[0]
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				res, err := c.MintNodeKey(ctx, req)
				if err != nil {
					return err
				}
				if !p.table() {
					return p.data(res)
				}
				fmt.Fprintf(a.stderr, "Node key %s (id %d) minted. It is shown only now:\n", args[0], res.GetId())
				fmt.Fprintln(a.stdout, res.GetPlainKey())
				return nil
			})
		},
	}
	outputFlag(mint, &format)
	mint.Flags().StringVar(&req.NodeId, "node-id", "", "the node id the key registers")
	_ = mint.MarkFlagRequired("node-id")
	mint.Flags().StringSliceVar(&req.AllowedCidrs, "allowed-cidrs", nil, "networks the node may register from")
	mint.Flags().StringSliceVar(&req.AllowedModels, "allowed-models", nil, "models the node may serve")
	mint.Flags().Float64Var(&req.MaxBaseWeight, "max-base-weight", 0, "cap on the weight the node may claim")
	mint.Flags().Int32Var(&req.MaxConcurrency, "max-concurrency", 0, "cap on the concurrency the node may claim")

	var yes bool
	revoke := &cobra.Command{
		Use: "revoke ID|NAME", Short: "Revoke a node key", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				err = a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
					res, err := c.ListNodeKeys(ctx, &pb.ListNodeKeysRequest{})
					if err != nil {
						return err
					}
					var ids []int64
					for _, k := range res.GetKeys() {
						if k.GetName() == args[0] && !k.GetDisabled() {
							ids = append(ids, k.GetId())
						}
					}
					switch len(ids) {
					case 0:
						return fmt.Errorf("no active node key %q", args[0])
					case 1:
						id = ids[0]
						return nil
					}
					return errors.New("several node keys are named " + args[0] + "; revoke by id")
				})
				if err != nil {
					return err
				}
			}
			// Asked outside a.call, so time at the prompt does not eat the deadline.
			if !yes {
				if err := a.confirm(fmt.Sprintf("Revoke node key %s (id %d)?", args[0], id)); err != nil {
					return err
				}
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				if _, err := c.RevokeNodeKey(ctx, &pb.RevokeNodeKeyRequest{Id: id}); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Revoked node key %d.\n", id)
				return nil
			})
		},
	}
	revoke.Flags().BoolVar(&yes, "yes", false, "revoke without asking")
	cmd.AddCommand(ls, mint, revoke)
	return cmd
}

func (a *app) rotateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rotate-encryption",
		Short: "Re-encrypt vendor keys and node tokens with the active keyring version",
		Long:  "Re-encrypt every vendor key and node auth token with the active KTMS_ENCRYPTION_KEYS version. Running it again changes nothing.",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				res, err := c.RotateEncryption(ctx, &pb.RotateEncryptionRequest{})
				if err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Re-encrypted %d secrets.\n", res.GetReencrypted())
				return nil
			})
		},
	}
}

func (a *app) vendorKeysCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "vendor-keys", Short: "Vendor credentials of client keys"}
	var format string
	ls := &cobra.Command{
		Use: "list", Short: "List vendor keys by fingerprint (never the keys themselves)", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				cfg, err := c.GetConfig(ctx, &pb.GetConfigRequest{})
				if err != nil {
					return err
				}
				if !p.table() {
					return p.data(messages(cfg.GetVendorKeys()))
				}
				var rows [][]string
				for _, v := range cfg.GetVendorKeys() {
					rows = append(rows, []string{v.GetApiKeyName(), v.GetVendor(), orDash(v.GetFingerprint()), when(v.GetCreatedAt())})
				}
				return p.rows([]string{"CLIENT KEY", "VENDOR", "FINGERPRINT", "CREATED"}, rows)
			})
		},
	}
	outputFlag(ls, &format)
	set := &cobra.Command{
		Use:   "set CLIENT_KEY VENDOR",
		Short: "Give a client key a vendor credential, read from stdin",
		Long:  "Read the vendor's API key from stdin (without echo on a terminal) and store it, encrypted, for the client key.",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			plain, err := a.readSecret(args[1] + " key: ")
			if err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				if _, err := c.SetVendorKey(ctx, &pb.SetVendorKeyRequest{ApiKeyName: args[0], Vendor: args[1], PlainKey: plain}); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Vendor key %s/%s stored.\n", args[0], args[1])
				return nil
			})
		},
	}
	del := &cobra.Command{
		Use: "delete CLIENT_KEY VENDOR", Short: "Remove a client key's vendor credential", Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				if _, err := c.DeleteVendorKey(ctx, &pb.DeleteVendorKeyRequest{ApiKeyName: args[0], Vendor: args[1]}); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Vendor key %s/%s removed.\n", args[0], args[1])
				return nil
			})
		},
	}
	cmd.AddCommand(ls, set, del)
	return cmd
}

var efforts = map[string]pb.Effort{"low": pb.Effort_EFFORT_LOW, "medium": pb.Effort_EFFORT_MEDIUM, "high": pb.Effort_EFFORT_HIGH}

// explainRequest builds ExplainRoute's request from names a person types.
func explainRequest(key, effort string, auto bool, attrs map[string]string, placement, model string, caps []string, dataClass string) (*pb.ExplainRouteRequest, error) {
	req := &pb.ExplainRouteRequest{ApiKeyName: key, AutoRoute: auto, Attributes: attrs, Placement: placement, ModelOverride: model, Capabilities: caps,
		DataClassification: dataClass}
	if effort != "" {
		e, ok := efforts[effort]
		if !ok {
			return nil, usageError{fmt.Errorf("effort %q: want low, medium or high", effort)}
		}
		req.Effort = e
	}
	return req, nil
}

func (a *app) routeCmd() *cobra.Command {
	var effort, placement, model, dataClass, format string
	var auto bool
	var attrs map[string]string
	var caps []string
	explain := &cobra.Command{
		Use:   "explain CLIENT_KEY [--effort E | --auto] [--attr k=v] [--placement P] [--model M] [--capability C]",
		Short: "Show where a request with this key would go, and why each model is kept or dropped",
		Long: `Dry-run routing on the server, on the same code as a real call: the rule a request
would match and every model of its chain, kept or dropped with the reason
(no vendor key, key or team policy, capability, placement, circuit open, vendor
paused, no node). It calls no vendor or judge and spends no limit.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			req, err := explainRequest(args[0], effort, auto, attrs, placement, model, caps, dataClass)
			if err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				res, err := c.ExplainRoute(ctx, req)
				if err != nil {
					return err
				}
				if !p.table() {
					return p.data(res)
				}
				rule := orDash(res.GetRule())
				fmt.Fprintf(a.stdout, "rule: %s   effort: %s   route mode: %s\n", rule, orDash(res.GetEffort()), orDash(res.GetRouteMode()))
				if res.GetJudgeSkipped() {
					fmt.Fprintln(a.stdout, "(auto route without an effort: a real call asks the judge first; shown with its fallback, medium)")
				}
				var rows [][]string
				for i, c := range res.GetCandidates() {
					state := "kept"
					if c.GetDropped() != "" {
						state = "dropped: " + c.GetDropped()
						if d := c.GetPausedFor(); d != nil {
							state += fmt.Sprintf(" (%s left)", d.AsDuration().Round(time.Second))
						}
					}
					rows = append(rows, []string{strconv.Itoa(i + 1), c.GetModelId(), orDash(c.GetVendor()), state})
				}
				if err := p.rows([]string{"#", "MODEL", "VENDOR", "STATE"}, rows); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "tries: %s\n", orDash(strings.Join(res.GetModels(), " → ")))
				if res.GetShadowModel() != "" {
					fmt.Fprintf(a.stdout, "shadow: %s at %g%%\n", res.GetShadowModel(), res.GetShadowPercentage())
				}
				if res.GetError() != "" {
					fmt.Fprintf(a.stdout, "a call would fail: %s\n", res.GetError())
				}
				return nil
			})
		},
	}
	f := explain.Flags()
	f.StringVar(&effort, "effort", "", "low, medium or high")
	f.BoolVar(&auto, "auto", false, "auto route, as model auto on /v1")
	f.StringToStringVar(&attrs, "attr", nil, "request attribute key=value, matched by rules")
	f.StringVar(&placement, "placement", "", "nodes_first, nodes_only, cloud_first or cloud_only")
	f.StringVar(&model, "model", "", "a model override instead of the rules")
	f.StringSliceVar(&caps, "capability", nil, "capabilities the request needs, e.g. google_search")
	f.StringVar(&dataClass, "data-classification", "", "restricted: the request stays on self-hosted nodes")
	_ = explain.RegisterFlagCompletionFunc("effort", cobra.FixedCompletions([]string{"low", "medium", "high"}, cobra.ShellCompDirectiveNoFileComp))
	outputFlag(explain, &format)
	cmd := &cobra.Command{Use: "route", Short: "Routing"}
	cmd.AddCommand(explain)
	return cmd
}
