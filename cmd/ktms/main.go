// Command ktms administers a Keystone (KTMS) gateway: its configuration as a
// file, keys, spend, limits and nodes through the Admin API, and a test call
// and health checks through the HTTP API.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// app is one invocation: the streams and the connection flags.
type app struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	t              target
	timeout        time.Duration
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &app{stdin: stdin, stdout: stdout, stderr: stderr}
	a.t.warn = stderr
	root := a.root()
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.Execute()
	if err == nil {
		return exitOK
	}
	var c codeError
	if errors.As(err, &c) {
		return c.code
	}
	fmt.Fprintln(stderr, "ktms:", errText(err))
	if isUsage(err) {
		return exitUsage
	}
	return exitCode(err)
}

// codeError ends the command with a code and nothing more to print.
type codeError struct{ code int }

func (e codeError) Error() string { return fmt.Sprintf("exit %d", e.code) }

// isUsage tells cobra's own argument errors from the API's.
func isUsage(err error) bool {
	var u usageError
	if errors.As(err, &u) {
		return true
	}
	msg := err.Error()
	for _, p := range []string{"unknown command", "accepts ", "requires at least", "required flag", "if any flags in the group"} {
		if strings.HasPrefix(msg, p) {
			return true
		}
	}
	return false
}

func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:   "ktms",
		Short: "Administer a Keystone (KTMS) gateway",
		Long: `ktms administers a Keystone (KTMS) gateway.

Connection: --context, or --addr (KTMS_ADDR, default localhost:9090) and --tls
(KTMS_TLS=true); the HTTP API at --http (KTMS_HTTP, default port 8080 of the
same host). The admin key comes from --key-file, KTMS_ADMIN_KEY or the
context; admin:read is enough for every command that only reads.

` + exitCodesHelp,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			a.t.tlsSet = cmd.Flags().Changed("tls")
		},
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	pf := root.PersistentFlags()
	pf.StringVar(&a.t.context, "context", "", "context from ~/.config/ktms/config.yaml (KTMS_CONTEXT)")
	pf.StringVar(&a.t.addr, "addr", "", "Admin API address host:port (KTMS_ADDR)")
	pf.BoolVar(&a.t.tls, "tls", false, "use TLS to the Admin API (KTMS_TLS)")
	pf.StringVar(&a.t.http, "http", "", "HTTP API base URL (KTMS_HTTP)")
	pf.StringVar(&a.t.keyFile, "key-file", "", "read the admin key from this file instead of KTMS_ADMIN_KEY")
	pf.DurationVar(&a.timeout, "timeout", time.Minute, "time limit for each call")

	root.AddCommand(
		a.configCmd(), a.contextCmd(),
		a.modelsCmd(), a.rulesCmd(), a.keysCmd(), a.vendorKeysCmd(), a.orgsCmd(), a.teamsCmd(),
		a.spendCmd(), a.limitsCmd(), a.nodesCmd(), a.nodeKeysCmd(), a.rotateCmd(),
		a.chatCmd(), a.healthCmd(), &cobra.Command{
			Use: "version", Short: "Print the version", Args: cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error {
				fmt.Fprintln(a.stdout, version)
				return nil
			},
		},
	)
	return root
}

// ctx bounds one command's calls by --timeout.
func (a *app) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), a.timeout)
}

// confirm asks on a terminal; elsewhere the command needs --yes, so a script
// never hangs on a prompt nobody sees.
func (a *app) confirm(question string) error {
	f, ok := a.stdin.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return usageError{errors.New("not a terminal: pass --yes")}
	}
	fmt.Fprintf(a.stdout, "%s [y/N] ", question)
	line, _ := bufio.NewReader(a.stdin).ReadString('\n')
	if s := strings.ToLower(strings.TrimSpace(line)); s != "y" && s != "yes" {
		fmt.Fprintln(a.stdout, "Nothing done.")
		return codeError{exitFailed}
	}
	return nil
}

// outputFlag adds -o to a command that prints data.
func outputFlag(cmd *cobra.Command, dst *string) {
	cmd.Flags().StringVarP(dst, "output", "o", "table", "output format: table, json or yaml")
	_ = cmd.RegisterFlagCompletionFunc("output", cobra.FixedCompletions([]string{"table", "json", "yaml"}, cobra.ShellCompDirectiveNoFileComp))
}
