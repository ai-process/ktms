package declarative

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Export writes the live configuration in the file format. Secrets are not
// exported: each vendor key and MCP credential becomes a reference to an
// environment variable to fill in, and
// client keys are listed without theirs. Revoked keys are left out.
func Export(live *State, w io.Writer) error {
	doc, err := exportDoc(live)
	if err != nil {
		return err
	}
	return encode(doc, w)
}

// ExportListed is Export without the sections that list nothing, for a new
// file: a section it leaves out stays unmanaged, so --prune never empties it.
func ExportListed(live *State, w io.Writer) error {
	doc, err := exportDoc(live)
	if err != nil {
		return err
	}
	kept := doc.Content[:0]
	for i := 0; i < len(doc.Content); i += 2 {
		if v := doc.Content[i+1]; v.Kind == yaml.SequenceNode && len(v.Content) == 0 {
			continue
		}
		kept = append(kept, doc.Content[i], doc.Content[i+1])
	}
	doc.Content = kept
	return encode(doc, w)
}

func exportDoc(live *State) (*yaml.Node, error) {
	doc := mapping()
	add(doc, "api_version", scalarNode(APIVersion))

	models := seq()
	ms := slices.Clone(live.Config.GetModels())
	sort.Slice(ms, func(i, j int) bool { return ms[i].GetId() < ms[j].GetId() })
	for _, m := range ms {
		n, err := messageNode(m, true)
		if err != nil {
			return nil, err
		}
		models.Content = append(models.Content, n)
	}
	add(doc, "models", models)

	rules := seq()
	rs := slices.Clone(live.Config.GetRules())
	sort.Slice(rs, func(i, j int) bool { return rs[i].GetPriority() > rs[j].GetPriority() })
	for _, r := range rs {
		n, err := messageNode(r, true)
		if err != nil {
			return nil, err
		}
		rules.Content = append(rules.Content, n)
	}
	add(doc, "rules", rules)

	orgs := seq()
	for _, o := range live.Orgs {
		on := mapping()
		add(on, "name", scalarNode(o.GetName()))
		teams := seq()
		for _, t := range live.Teams {
			if t.GetOrgId() != o.GetId() {
				continue
			}
			tn := mapping()
			add(tn, "name", scalarNode(t.GetName()))
			if t.GetMonthlyBudgetUsd() > 0 {
				add(tn, "monthly_budget_usd", numberNode(fmt.Sprint(t.GetMonthlyBudgetUsd())))
			}
			if len(t.GetAllowedModels()) > 0 {
				add(tn, "allowed_models", stringSeq(t.GetAllowedModels()))
			}
			if len(t.GetAllowedMcp()) > 0 {
				add(tn, "allowed_mcp", stringSeq(t.GetAllowedMcp()))
			}
			teams.Content = append(teams.Content, tn)
		}
		if len(teams.Content) > 0 {
			add(on, "teams", teams)
		}
		orgs.Content = append(orgs.Content, on)
	}
	add(doc, "organizations", orgs)

	keys := seq()
	for _, k := range live.Keys {
		if k.GetDisabled() {
			continue
		}
		kn := mapping()
		add(kn, "name", scalarNode(k.GetName()))
		add(kn, "scopes", stringSeq(k.GetScopes()))
		org, team := keyTenancy(live, k)
		if org != "" {
			add(kn, "organization", scalarNode(org))
		}
		if team != "" {
			add(kn, "team", scalarNode(team))
		}
		if k.GetPolicy() != nil {
			pn, err := messageNode(k.GetPolicy(), false)
			if err != nil {
				return nil, err
			}
			add(kn, "policy", pn)
		}
		keys.Content = append(keys.Content, kn)
	}
	add(doc, "api_keys", keys)

	vks := seq()
	for _, v := range live.Config.GetVendorKeys() {
		if k := live.key(v.GetApiKeyName()); k == nil || k.GetDisabled() {
			continue // a revoked key's credential cannot be used, nor its key recreated
		}
		vn := mapping()
		add(vn, "api_key_name", scalarNode(v.GetApiKeyName()))
		add(vn, "vendor", scalarNode(v.GetVendor()))
		add(vn, "plain_key_env", scalarNode(envName(v.GetApiKeyName(), v.GetVendor())))
		vks.Content = append(vks.Content, vn)
	}
	add(doc, "vendor_keys", vks)

	limits := seq()
	for _, l := range live.Config.GetSpendLimits() {
		if l.GetSubjectType() == "team" {
			continue // exported as the team's monthly_budget_usd
		}
		ln := mapping()
		add(ln, "subject_type", scalarNode(l.GetSubjectType()))
		add(ln, "subject_id", scalarNode(l.GetSubjectId()))
		add(ln, "period", scalarNode(l.GetPeriod()))
		add(ln, "limit_nanousd", numberNode(fmt.Sprint(l.GetLimitNanousd())))
		limits.Content = append(limits.Content, ln)
	}
	add(doc, "spend_limits", limits)

	urls := seq()
	for _, u := range live.Config.GetUserRateLimits() {
		n, err := messageNode(u, false)
		if err != nil {
			return nil, err
		}
		urls.Content = append(urls.Content, n)
	}
	add(doc, "user_rate_limits", urls)

	servers := seq()
	for _, m := range live.Config.GetMcpServers() {
		n, err := messageNode(m, true)
		if err != nil {
			return nil, err
		}
		servers.Content = append(servers.Content, n)
	}
	add(doc, "mcp_servers", servers)

	creds := seq()
	for _, mc := range live.Config.GetMcpCredentials() {
		cn := mapping()
		add(cn, "server", scalarNode(mc.GetServer()))
		add(cn, "header", scalarNode(mc.GetHeader()))
		add(cn, "value_env", scalarNode(mcpEnvName(mc.GetServer())))
		creds.Content = append(creds.Content, cn)
	}
	add(doc, "mcp_credentials", creds)

	// A Keystone that predates node schedules exports without the section.
	if live.NodeSchedulesErr == nil {
		scheds := seq()
		for _, ns := range live.NodeSchedules {
			if ns.GetVersion() == 0 || strings.TrimSpace(ns.GetText()) == "" {
				continue
			}
			n := mapping()
			add(n, "node_id", scalarNode(ns.GetNodeId()))
			add(n, "schedule", literalNode(ns.GetText()))
			scheds.Content = append(scheds.Content, n)
		}
		add(doc, "node_schedules", scheds)
	}

	return doc, nil
}

func encode(doc *yaml.Node, w io.Writer) error {
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{doc}}); err != nil {
		return err
	}
	return enc.Close()
}

// mcpEnvName is the variable an exported MCP credential points at, e.g.
// KTMS_MCP_CREDENTIAL_GITHUB.
func mcpEnvName(server string) string {
	return "KTMS_MCP_CREDENTIAL_" + strings.ToUpper(strings.ReplaceAll(server, "-", "_"))
}

// envName is the variable an exported vendor key points at, e.g.
// KTMS_VENDOR_KEY_BILLING_SERVICE_OPENAI.
func envName(key, vendor string) string {
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z':
				return r - 'a' + 'A'
			case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				return r
			}
			return '_'
		}, s)
	}
	return "KTMS_VENDOR_KEY_" + clean(key) + "_" + clean(vendor)
}

// messageNode renders a message with its fields in declaration order.
// withEnabled writes "enabled" even when false, since a missing one parses as true.
func messageNode(m proto.Message, withEnabled bool) (*yaml.Node, error) {
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	if err != nil {
		return nil, err
	}
	n, err := jsonNode(json.NewDecoder(bytes.NewReader(b)))
	if err != nil {
		return nil, err
	}
	if withEnabled && n.Kind == yaml.MappingNode {
		has := false
		for i := 0; i < len(n.Content); i += 2 {
			has = has || n.Content[i].Value == "enabled"
		}
		if !has {
			add(n, "enabled", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"})
		}
	}
	return n, nil
}

// jsonNode turns JSON into YAML nodes, keeping the order of object keys.
func jsonNode(d *json.Decoder) (*yaml.Node, error) {
	d.UseNumber()
	tok, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '{' {
			n := mapping()
			for d.More() {
				kt, err := d.Token()
				if err != nil {
					return nil, err
				}
				v, err := jsonNode(d)
				if err != nil {
					return nil, err
				}
				add(n, kt.(string), v)
			}
			_, err := d.Token()
			return n, err
		}
		n := seq()
		for d.More() {
			v, err := jsonNode(d)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, v)
		}
		_, err := d.Token()
		return n, err
	case string:
		return scalarNode(t), nil
	case json.Number:
		return numberNode(t.String()), nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(t)}, nil
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	}
	return nil, fmt.Errorf("unexpected JSON token %v", tok)
}

func mapping() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode} }
func seq() *yaml.Node     { return &yaml.Node{Kind: yaml.SequenceNode} }
func scalarNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

// literalNode keeps a multi-line string readable as a YAML block.
func literalNode(v string) *yaml.Node {
	n := scalarNode(v)
	if strings.Contains(v, "\n") {
		n.Style = yaml.LiteralStyle
	}
	return n
}

func numberNode(v string) *yaml.Node {
	tag := "!!int"
	if strings.ContainsAny(v, ".eE") {
		tag = "!!float"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: v}
}
func stringSeq(vs []string) *yaml.Node {
	n := seq()
	n.Style = yaml.FlowStyle
	for _, v := range vs {
		n.Content = append(n.Content, scalarNode(v))
	}
	return n
}
func add(m *yaml.Node, k string, v *yaml.Node) {
	m.Content = append(m.Content, scalarNode(k), v)
}
