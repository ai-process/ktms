package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func (a *app) contextCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "Name Keystones and where their keys live (~/.config/ktms/config.yaml)",
	}
	complete := func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		cs, err := loadContexts()
		if err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		return cs.names(), cobra.ShellCompDirectiveNoFileComp
	}

	var format string
	list := &cobra.Command{
		Use: "list", Short: "List contexts; * marks the current one", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			cs, err := loadContexts()
			if err != nil {
				return err
			}
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			if !p.table() {
				return p.data(cs)
			}
			var rows [][]string
			for _, n := range cs.names() {
				c, cur := cs.Contexts[n], " "
				if n == cs.Current {
					cur = "*"
				}
				rows = append(rows, []string{cur, n, c.Addr, yesNo(c.TLS), orDash(c.HTTP), c.Key.String(), c.ClientKey.String()})
			}
			return p.rows([]string{"", "NAME", "ADDR", "TLS", "HTTP", "KEY", "CLIENT KEY"}, rows)
		},
	}
	outputFlag(list, &format)

	use := &cobra.Command{
		Use: "use NAME", Short: "Make a context the current one", Args: cobra.ExactArgs(1), ValidArgsFunction: complete,
		RunE: func(_ *cobra.Command, args []string) error {
			cs, err := loadContexts()
			if err != nil {
				return err
			}
			if cs.Contexts[args[0]] == nil {
				return fmt.Errorf("no context %q", args[0])
			}
			cs.Current = args[0]
			return cs.save()
		},
	}

	var addr, httpURL, key, clientKey string
	var useTLS bool
	set := &cobra.Command{
		Use:   "set NAME [--addr HOST:PORT] [--tls] [--http URL] [--key SOURCE] [--client-key SOURCE]",
		Short: "Create or change a context",
		Long: `Create or change a context. A key SOURCE is env:VAR, file:PATH or keychain
(macOS Keychain or the Linux secret service; fill it with ktms context store-key).
The first context created becomes the current one.`,
		Args: cobra.ExactArgs(1), ValidArgsFunction: complete,
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, err := loadContexts()
			if err != nil {
				return err
			}
			c := cs.Contexts[args[0]]
			if c == nil {
				c = &Context{}
				cs.Contexts[args[0]] = c
			}
			f := cmd.Flags()
			if f.Changed("addr") {
				c.Addr = addr
			}
			if f.Changed("tls") {
				c.TLS = useTLS
			}
			if f.Changed("http") {
				c.HTTP = httpURL
			}
			if f.Changed("key") {
				if c.Key, err = parseKeySource(key); err != nil {
					return usageError{err}
				}
			}
			if f.Changed("client-key") {
				if c.ClientKey, err = parseKeySource(clientKey); err != nil {
					return usageError{err}
				}
			}
			if cs.Current == "" {
				cs.Current = args[0]
			}
			return cs.save()
		},
	}
	// Local flags shadow the global connection flags: here they describe the context.
	set.Flags().StringVar(&addr, "addr", "", "Admin API address host:port")
	set.Flags().BoolVar(&useTLS, "tls", false, "use TLS")
	set.Flags().StringVar(&httpURL, "http", "", "HTTP API base URL")
	set.Flags().StringVar(&key, "key", "", "admin key source: env:VAR, file:PATH or keychain")
	set.Flags().StringVar(&clientKey, "client-key", "", "client key source for ktms chat")

	del := &cobra.Command{
		Use: "delete NAME", Short: "Remove a context (keychain entries stay)", Args: cobra.ExactArgs(1), ValidArgsFunction: complete,
		RunE: func(_ *cobra.Command, args []string) error {
			cs, err := loadContexts()
			if err != nil {
				return err
			}
			if cs.Contexts[args[0]] == nil {
				return fmt.Errorf("no context %q", args[0])
			}
			delete(cs.Contexts, args[0])
			if cs.Current == args[0] {
				cs.Current = ""
			}
			return cs.save()
		},
	}

	var client, toFile bool
	store := &cobra.Command{
		Use:   "store-key NAME [--client] [--file]",
		Short: "Read a key from stdin and keep it in the keychain (or a 0600 file)",
		Long: `Read a key from stdin, without echo on a terminal, and keep it in the system
keychain; --file keeps it in ~/.config/ktms/NAME.key (mode 0600) instead. The
context's key source is updated to match. --client stores the client key used
by ktms chat.`,
		Args: cobra.ExactArgs(1), ValidArgsFunction: complete,
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			cs, err := loadContexts()
			if err != nil {
				return err
			}
			c := cs.Contexts[name]
			if c == nil {
				return fmt.Errorf("no context %q (ktms context set %s first)", name, name)
			}
			plain, err := a.readSecret("Key: ")
			if err != nil {
				return err
			}
			account, suffix := name, ""
			if client {
				account, suffix = name+"/client", ".client"
			}
			var src KeySource
			if toFile {
				dir, err := configDir()
				if err != nil {
					return err
				}
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return err
				}
				path := filepath.Join(dir, safeName(name)+suffix+".key")
				if err := writePrivate(path, []byte(plain+"\n")); err != nil {
					return err
				}
				src = KeySource{File: path}
			} else {
				if err := keychainSet(account, plain); err != nil {
					return fmt.Errorf("%w (pass --file to keep the key in a file instead)", err)
				}
				src = KeySource{Keychain: true}
			}
			if client {
				c.ClientKey = src
			} else {
				c.Key = src
			}
			if err := cs.save(); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Stored the %s key of %s in %s.\n", map[bool]string{false: "admin", true: "client"}[client], name, src)
			return nil
		},
	}
	store.Flags().BoolVar(&client, "client", false, "store the client key for ktms chat")
	store.Flags().BoolVar(&toFile, "file", false, "keep the key in a 0600 file instead of the keychain")

	cmd.AddCommand(list, use, set, del, store)
	return cmd
}

// readSecret reads one line from stdin, without echo on a terminal.
func (a *app) readSecret(prompt string) (string, error) {
	var line string
	if f, ok := a.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(a.stderr, prompt)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(a.stderr)
		if err != nil {
			return "", err
		}
		line = string(b)
	} else {
		s, err := bufio.NewReader(a.stdin).ReadString('\n')
		if err != nil && s == "" {
			return "", errors.New("no key on stdin")
		}
		line = s
	}
	if line = strings.TrimSpace(line); line == "" {
		return "", errors.New("empty key")
	}
	return line, nil
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == os.PathSeparator {
			return '_'
		}
		return r
	}, s)
}
