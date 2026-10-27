package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/spf13/cobra"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

func (a *app) mcpServersCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "mcp-servers", Short: "MCP servers proxied at /mcp, and their credentials"}

	var format string
	ls := &cobra.Command{
		Use: "list", Short: "List MCP servers (never their credentials)", Args: cobra.NoArgs,
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
				ss := cfg.GetMcpServers()
				sort.Slice(ss, func(i, j int) bool { return ss[i].GetName() < ss[j].GetName() })
				if !p.table() {
					return p.data(map[string]any{"servers": messages(ss), "credentials": messages(cfg.GetMcpCredentials())})
				}
				creds := map[string]*pb.MCPCredentialInfo{}
				for _, mc := range cfg.GetMcpCredentials() {
					creds[mc.GetServer()] = mc
				}
				var rows [][]string
				for _, s := range ss {
					cred := "-"
					if mc := creds[s.GetName()]; mc != nil {
						cred = mc.GetHeader() + " " + orDash(mc.GetFingerprint())
					}
					rows = append(rows, []string{s.GetName(), s.GetUrl(), s.GetTransport(), yesNo(s.GetAllowPrivate()),
						limit(int64(s.GetToolRpm())), cred, yesNo(s.GetEnabled())})
				}
				return p.rows([]string{"NAME", "URL", "TRANSPORT", "PRIVATE", "TOOL RPM", "CREDENTIAL", "ENABLED"}, rows)
			})
		},
	}
	outputFlag(ls, &format)

	spec := &pb.MCPServerSpec{}
	var disabled bool
	set := &cobra.Command{
		Use:   "set NAME --url URL [--transport sse] [--allow-private] [--timeout-ms N] [--max-response-bytes N] [--tool-rpm N]",
		Short: "Register an MCP server or replace its settings",
		Long: `Register an MCP server or replace its settings; flags left out take their defaults.
Its tools appear on /mcp as NAME__tool and on /mcp/NAME as they are. A host that is
or resolves to a private address is refused unless --allow-private is given.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if spec.GetUrl() == "" {
				return usageError{fmt.Errorf("--url is required")}
			}
			spec.Name, spec.Enabled = args[0], !disabled
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				if _, err := c.UpsertMCPServer(ctx, &pb.UpsertMCPServerRequest{Server: spec}); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "MCP server %s registered.\n", args[0])
				return nil
			})
		},
	}
	set.Flags().StringVar(&spec.Url, "url", "", "the server's MCP endpoint")
	set.Flags().StringVar(&spec.Transport, "transport", "streamable_http", "streamable_http or sse")
	set.Flags().BoolVar(&spec.AllowPrivate, "allow-private", false, "permit a private, loopback or link-local address")
	set.Flags().Int32Var(&spec.TimeoutMs, "timeout-ms", 0, "one tool call (default 60000)")
	set.Flags().Int64Var(&spec.MaxResponseBytes, "max-response-bytes", 0, "one tool result (default 4 MiB)")
	set.Flags().Int32Var(&spec.ToolRpm, "tool-rpm", 0, "calls per minute per client key and tool (default unlimited)")
	set.Flags().BoolVar(&disabled, "disabled", false, "register it switched off")

	var yes bool
	del := &cobra.Command{
		Use: "delete NAME", Short: "Remove an MCP server and its credential", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if !yes {
				if err := a.confirm("Remove MCP server " + args[0] + "?"); err != nil {
					return err
				}
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				if _, err := c.DeleteMCPServer(ctx, &pb.DeleteMCPServerRequest{Name: args[0]}); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "MCP server %s removed.\n", args[0])
				return nil
			})
		},
	}
	del.Flags().BoolVar(&yes, "yes", false, "remove without asking")

	var header string
	credSet := &cobra.Command{
		Use:   "set NAME [--header H]",
		Short: "Give an MCP server the header value the gateway sends it, read from stdin",
		Long: `Read the value from stdin (without echo on a terminal), such as "Bearer <token>",
and store it encrypted. The gateway adds it to every request to the server; clients
never see it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			plain, err := a.readSecret(header + ": ")
			if err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				if _, err := c.SetMCPCredential(ctx, &pb.SetMCPCredentialRequest{Server: args[0], Header: header, PlainValue: plain}); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Credential of MCP server %s stored.\n", args[0])
				return nil
			})
		},
	}
	credSet.Flags().StringVar(&header, "header", "Authorization", "HTTP header to send")
	credDel := &cobra.Command{
		Use: "delete NAME", Short: "Remove an MCP server's credential", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				if _, err := c.DeleteMCPCredential(ctx, &pb.DeleteMCPCredentialRequest{Server: args[0]}); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Credential of MCP server %s removed.\n", args[0])
				return nil
			})
		},
	}
	cred := &cobra.Command{Use: "credential", Short: "The header value the gateway sends a server"}
	cred.AddCommand(credSet, credDel)

	var toolsFormat string
	tools := &cobra.Command{
		Use: "tools NAME", Short: "Ask a server for its tools through the gateway", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := newPrinter(a.stdout, toolsFormat)
			if err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				res, err := c.ListMCPTools(ctx, &pb.ListMCPToolsRequest{Server: args[0]})
				if err != nil {
					return err
				}
				if !p.table() {
					return p.data(messages(res.GetTools()))
				}
				var rows [][]string
				for _, t := range res.GetTools() {
					rows = append(rows, []string{args[0] + "/" + t.GetName(), args[0] + "__" + t.GetName(), t.GetDescription()})
				}
				fmt.Fprintln(a.stderr, strconv.Itoa(len(rows))+" tools; allowed_mcp takes the first column, /mcp shows the second.")
				return p.rows([]string{"ALLOWED_MCP", "ON /mcp", "DESCRIPTION"}, rows)
			})
		},
	}
	outputFlag(tools, &toolsFormat)

	cmd.AddCommand(ls, set, del, cred, tools)
	return cmd
}
