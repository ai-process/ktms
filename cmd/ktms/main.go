// Command ktms administers a Keystone (KTMS) gateway through its Admin API.
// This first version manages the configuration as a file: validate, plan,
// apply, export and the file's JSON Schema.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/ai-process/ktms/internal/declarative"
)

const usage = `Usage: ktms <command> [flags]

Commands:
  config validate -f FILE           check a configuration file
  config plan     -f FILE [--prune] show what apply would change
  config apply    -f FILE [--prune] [--yes]
                                    change Keystone to match the file
  config export   [-o FILE]         write the live configuration as a file
  config schema                     print the file's JSON Schema
  version                           print the version

Connection: --addr (KTMS_ADDR, default localhost:9090), --tls (KTMS_TLS=true),
the admin key in KTMS_ADMIN_KEY or --key-file (admin:read is enough for plan
and export).
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// Exit codes: 0 done, 1 failed, 2 bad usage; plan --detailed-exitcode exits 3
// when there are changes, for drift checks in CI.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Fprintln(stdout, version)
		return 0
	}
	if args[0] != "config" || len(args) < 2 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	sub, rest := args[1], args[2:]
	fs := flag.NewFlagSet("ktms config "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	t := target{}
	fs.StringVar(&t.addr, "addr", envOr("KTMS_ADDR", "localhost:9090"), "Admin API address host:port")
	fs.BoolVar(&t.tls, "tls", os.Getenv("KTMS_TLS") == "true", "use TLS")
	fs.StringVar(&t.keyFile, "key-file", "", "read the admin key from this file instead of KTMS_ADMIN_KEY")
	file := fs.String("f", "", "configuration file")
	out := fs.String("o", "", "output file (export; default stdout)")
	prune := fs.Bool("prune", false, "delete what managed sections do not list")
	yes := fs.Bool("yes", false, "apply without asking")
	detailed := fs.Bool("detailed-exitcode", false, "plan: exit 3 when there are changes")
	timeout := fs.Duration("timeout", time.Minute, "time limit for reading and planning, and again for applying")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	// Time at the confirmation prompt must not eat into the apply's deadline.
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if !t.tls && !loopback(t.addr) && sub != "schema" && sub != "validate" {
		fmt.Fprintf(stderr, "ktms: warning: sending the admin key to %s without TLS; pass --tls unless this network is private\n", t.addr)
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "ktms:", err)
		return 1
	}
	load := func() (*declarative.Config, error) {
		if *file == "" {
			return nil, errors.New("-f FILE is required")
		}
		return declarative.Load(*file)
	}

	switch sub {
	case "schema":
		_, err := stdout.Write(declarative.Schema())
		if err != nil {
			return fail(err)
		}
		return 0
	case "validate":
		if _, err := load(); err != nil {
			return fail(err)
		}
		fmt.Fprintln(stdout, "The configuration is valid.")
		return 0
	case "export":
		c, closeConn, err := t.admin()
		if err != nil {
			return fail(err)
		}
		defer closeConn()
		live, err := declarative.Fetch(ctx, c)
		if err != nil {
			return fail(err)
		}
		w := stdout
		if *out != "" {
			f, err := os.Create(*out)
			if err != nil {
				return fail(err)
			}
			defer f.Close()
			w = f
		}
		if err := declarative.Export(live, w); err != nil {
			return fail(err)
		}
		return 0
	case "plan", "apply":
		cfg, err := load()
		if err != nil {
			return fail(err)
		}
		c, closeConn, err := t.admin()
		if err != nil {
			return fail(err)
		}
		defer closeConn()
		live, err := declarative.Fetch(ctx, c)
		if err != nil {
			return fail(err)
		}
		plan, err := declarative.Build(cfg, live, declarative.Options{Prune: *prune})
		if err != nil {
			return fail(err)
		}
		plan.Write(stdout)
		if sub == "plan" {
			if *detailed && !plan.Empty() {
				return 3
			}
			return 0
		}
		if plan.Empty() {
			return 0
		}
		if !*yes {
			ok, err := confirm(stdin, stdout)
			if err != nil {
				return fail(err)
			}
			if !ok {
				fmt.Fprintln(stdout, "Nothing applied.")
				return 1
			}
		}
		actx, acancel := context.WithTimeout(context.Background(), *timeout)
		defer acancel()
		res, err := declarative.Apply(actx, c, live, plan, stdout)
		printMinted(stdout, res)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(stdout, "Applied.")
		return 0
	}
	fmt.Fprint(stderr, usage)
	return 2
}

// confirm asks on a terminal; elsewhere apply needs --yes, so a script never
// hangs on a prompt nobody sees.
func confirm(stdin io.Reader, stdout io.Writer) (bool, error) {
	f, ok := stdin.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return false, errors.New("not a terminal: pass --yes to apply")
	}
	fmt.Fprint(stdout, "Apply these changes? [y/N] ")
	line, _ := bufio.NewReader(stdin).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes", nil
}

func printMinted(w io.Writer, res *declarative.Result) {
	if res == nil || len(res.Minted) == 0 {
		return
	}
	fmt.Fprintln(w, "\nNew client keys. They are shown only now; store them in your secret manager:")
	for name, key := range res.Minted {
		fmt.Fprintf(w, "  %s: %s\n", name, key)
	}
}

func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
