package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"google.golang.org/protobuf/proto"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/declarative"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/seed"
)

// initVendors are the vendors ktms init offers, with the variable their SDKs read.
var initVendors = []struct{ id, label, env string }{
	{registry.VendorOpenAI, "OpenAI", "OPENAI_API_KEY"},
	{registry.VendorGoogle, "Google Gemini", "GEMINI_API_KEY"},
	{registry.VendorAnthropic, "Anthropic", "ANTHROPIC_API_KEY"},
}

const schemaComment = "yaml-language-server: $schema=https://raw.githubusercontent.com/ai-process/ktms/main/schema/ktms-config.schema.json"

func (a *app) initCmd() *cobra.Command {
	var file string
	var yes bool
	cmd := &cobra.Command{
		Use:   "init [CONTEXT] [-f FILE]",
		Short: "Connect to a Keystone and give it a first configuration, step by step",
		Long: `Connect to a Keystone and give it a first configuration, asking step by step.

ktms init saves a context (default "local") with the admin key in the keychain
or a 0600 file and makes it current. On a Keystone without models it asks for
vendor keys and a client key name, writes FILE (default ktms.yaml) with the
default models and rules of those vendors, shows the plan and applies it. The
new client key is printed once and kept in the context for ktms chat. Vendor
keys go to Keystone only, never to the file.

On a Keystone that has models already, it writes the live configuration to
FILE instead, as ktms config export does.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := "local"
			if len(args) > 0 {
				name = args[0]
			}
			return a.runInit(name, file, yes)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "ktms.yaml", "configuration file to write")
	cmd.Flags().BoolVar(&yes, "yes", false, "apply without asking")
	_ = cmd.MarkFlagFilename("file", "yaml", "yml")
	return cmd
}

func (a *app) runInit(name, file string, yes bool) error {
	k := a.newAsker()
	cs, err := loadContexts()
	if err != nil {
		return err
	}
	c := cs.Contexts[name]
	reuse := false
	if c != nil {
		if reuse, err = k.yes(fmt.Sprintf("Context %s exists (%s). Use it as it is?", name, c.Addr), true); err != nil {
			return err
		}
	}
	if !reuse {
		if c, err = a.askContext(k, cs, name); err != nil {
			return err
		}
	}
	// Every call from here goes to this context, whatever the shell has set.
	a.t = target{context: name, warn: a.stderr}
	admin, closeConn, err := a.t.admin()
	if err != nil {
		return err
	}
	defer closeConn()
	ctx, cancel := a.ctx()
	live, err := declarative.Fetch(ctx, admin)
	cancel()
	if err != nil {
		return fmt.Errorf("%w (ktms init %s again to change the address or the key)", err, name)
	}

	if _, err := os.Stat(file); err == nil {
		ok, err := k.yes(file+" exists. Overwrite it?", false)
		if err != nil {
			return err
		}
		if !ok {
			return usageError{fmt.Errorf("%s exists: pass -f to write another file", file)}
		}
	}
	if n := len(live.Config.GetModels()); n > 0 {
		// Without vendor_keys, as the starter file: a plan needs no secrets.
		listed := *live
		listed.Config = proto.Clone(live.Config).(*pb.GetConfigResponse)
		listed.Config.VendorKeys = nil
		var buf bytes.Buffer
		fmt.Fprintf(&buf, "# %s\n# Written by ktms init. The vendor keys are in Keystone, not here: ktms vendor-keys list.\n", schemaComment)
		if err := declarative.ExportListed(&listed, &buf); err != nil {
			return err
		}
		if err := writeConfig(file, buf.Bytes()); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Keystone has %d models and %d rules already; wrote them to %s.\n", n, len(live.Config.GetRules()), file)
		fmt.Fprintf(a.stdout, "Next: edit it, then ktms config plan -f %s and ktms config apply -f %s.\n", file, file)
		return nil
	}

	vendorKeys := map[string]string{}
	var vendors []string
	for _, v := range initVendors {
		plain := ""
		if env := os.Getenv(v.env); env != "" {
			use, err := k.yes(fmt.Sprintf("Use %s from the environment for %s?", v.env, v.label), true)
			if err != nil {
				return err
			}
			if use {
				plain = env
			}
		}
		if plain == "" {
			if plain, err = k.secret(v.label + " API key (Enter to skip)"); err != nil {
				return err
			}
		}
		if plain != "" {
			vendorKeys[v.id] = plain
			vendors = append(vendors, v.id)
		}
	}
	if len(vendors) == 0 {
		return usageError{errors.New("no vendor key given: Keystone needs at least one to route to")}
	}
	keyName, err := k.line("Name of the first client key", "my-app")
	if err != nil {
		return err
	}

	body, err := starterConfig(vendors, keyName)
	if err != nil {
		return err
	}
	// The vendor keys are applied with the file but stay out of it: they live
	// in Keystone, and a file without the section leaves them alone.
	var vk strings.Builder
	envs := map[string]string{}
	vk.WriteString("vendor_keys:\n")
	for _, v := range vendors {
		env := "KTMS_INIT_VENDOR_KEY_" + strings.ToUpper(v)
		envs[env] = vendorKeys[v]
		fmt.Fprintf(&vk, "  - {api_key_name: %q, vendor: %s, plain_key_env: %s}\n", keyName, v, env)
	}
	cfg, err := declarative.Parse(append(append([]byte{}, body...), vk.String()...))
	if err != nil {
		return err
	}
	plan, err := declarative.Build(cfg, live, declarative.Options{Env: func(n string) (string, bool) {
		v, ok := envs[n]
		return v, ok
	}})
	if err != nil {
		return err
	}
	if err := writeConfig(file, body); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Wrote %s.\n\n", file)
	plan.Write(a.stdout)
	if !yes {
		ok, err := k.yes("Apply these changes?", false)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintf(a.stdout, "Nothing applied. The vendor keys are not saved anywhere; set them with ktms vendor-keys set %s VENDOR.\n", keyName)
			return nil
		}
	}
	actx, acancel := context.WithTimeout(context.Background(), a.timeout)
	defer acancel()
	res, err := declarative.Apply(actx, admin, live, plan, a.stdout)
	printMinted(a.stdout, res)
	if err != nil {
		return err
	}
	if plain := res.Minted[keyName]; plain != "" {
		src, err := storeKey(name, true, c.Key.File != "", plain)
		if err == nil {
			c.ClientKey = src
			err = cs.save()
		}
		if err != nil {
			fmt.Fprintf(a.stderr, "ktms: the client key was not kept in the context (%s); ktms context store-key %s --client\n", errText(err), name)
		} else {
			fmt.Fprintf(a.stdout, "\nKept the client key in %s for ktms chat.\n", src)
		}
	}
	fmt.Fprintf(a.stdout, "\nNext: ktms chat \"Say hi\" for a test call. Edit %[1]s, then\nktms config plan -f %[1]s and ktms config apply -f %[1]s.\n", file)
	return nil
}

// askContext asks where Keystone is and for its admin key, saves the context
// and makes it current.
func (a *app) askContext(k *asker, cs *Contexts, name string) (*Context, error) {
	addr, err := k.line("Admin API address (gRPC)", firstNonEmpty(a.t.addr, os.Getenv("KTMS_ADDR"), "localhost:9090"))
	if err != nil {
		return nil, err
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, usageError{fmt.Errorf("address %q: want host:port", addr)}
	}
	useTLS, err := k.yes("Use TLS?", a.t.tls || port == "443")
	if err != nil {
		return nil, err
	}
	httpURL := "http://" + net.JoinHostPort(host, "8080")
	if useTLS {
		httpURL = "https://" + host
		if port != "443" {
			httpURL = "https://" + net.JoinHostPort(host, "8080")
		}
	}
	if httpURL, err = k.line("HTTP API base URL", firstNonEmpty(a.t.http, os.Getenv("KTMS_HTTP"), httpURL)); err != nil {
		return nil, err
	}
	key, err := k.secret("Admin key (BOOTSTRAP_ADMIN_KEY on a new Keystone)")
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, usageError{errors.New("empty admin key")}
	}
	where, err := k.line("Keep the admin key in the keychain or a file", "keychain")
	if err != nil {
		return nil, err
	}
	if where != "keychain" && where != "file" {
		return nil, usageError{fmt.Errorf("%q: want keychain or file", where)}
	}
	src, err := storeKey(name, false, where == "file", key)
	if err != nil && where == "keychain" {
		fmt.Fprintf(a.stderr, "ktms: no keychain (%s); keeping the key in a file instead\n", errText(err))
		src, err = storeKey(name, false, true, key)
	}
	if err != nil {
		return nil, err
	}
	c := &Context{Addr: addr, TLS: useTLS, HTTP: strings.TrimRight(httpURL, "/"), Key: src}
	cs.Contexts[name] = c
	cs.Current = name
	if err := cs.save(); err != nil {
		return nil, err
	}
	fmt.Fprintf(a.stdout, "Saved context %s (admin key in %s) and made it current.\n", name, src)
	return c, nil
}

// starterConfig is the file ktms init writes: Keystone's default models of
// the chosen vendors, its default rules over them, and one client key.
func starterConfig(vendors []string, keyName string) ([]byte, error) {
	var models []*proxydb.Model
	for _, m := range seed.DefaultModels() {
		for _, v := range vendors {
			if m.Vendor == v {
				models = append(models, m)
			}
		}
	}
	cfg := &pb.GetConfigResponse{}
	for _, m := range models {
		cfg.Models = append(cfg.Models, modelSpec(m))
	}
	for _, r := range seed.DefaultRules(models, vendors) {
		cfg.Rules = append(cfg.Rules, &pb.RuleSpec{
			Name: r.Name, Priority: r.Priority, Effort: effortOf(r.Effort), Use: r.Use, Enabled: r.Enabled,
		})
	}
	state := &declarative.State{Config: cfg, Keys: []*pb.APIKeyInfo{{Name: keyName, Scopes: []string{apikeys.ScopeGenerate}}}}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# %s\n# Written by ktms init. Change it and run ktms config plan -f FILE, then apply.\n# The vendor keys are in Keystone, not here: ktms vendor-keys list.\n", schemaComment)
	if err := declarative.ExportListed(state, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func modelSpec(m *proxydb.Model) *pb.ModelSpec {
	s := &pb.ModelSpec{
		Id: m.ID, Vendor: m.Vendor, Endpoint: m.Endpoint, Capabilities: m.Capabilities, Rpm: m.RPM,
		PriceInPerMtok: m.PriceInPerMtok, PriceOutPerMtok: m.PriceOutPerMtok,
		PriceInPeakPerMtok: m.PriceInPeakPerMtok, PriceOutPeakPerMtok: m.PriceOutPeakPerMtok,
		PriceInBatchPerMtok: m.PriceInBatchPerMtok, PriceOutBatchPerMtok: m.PriceOutBatchPerMtok,
		PriceCacheReadPerMtok: m.PriceCacheReadPerMtok, PriceCacheWritePerMtok: m.PriceCacheWritePerMtok,
		DailyTokensPerKey: m.DailyTokensPerKey, DailyTokensPerUser: m.DailyTokensPerUser,
		RpmBurst: m.Burst, QueueTimeoutMs: m.QueueTimeoutMs, BudgetGraceMargin: m.BudgetGraceMargin,
		Enabled: m.Enabled,
	}
	for _, e := range m.Efforts {
		s.Efforts = append(s.Efforts, effortOf(e))
	}
	return s
}

func effortOf(e string) pb.Effort {
	if e == "" {
		return pb.Effort_EFFORT_UNSPECIFIED
	}
	return pb.Effort(pb.Effort_value["EFFORT_"+strings.ToUpper(e)])
}

func writeConfig(path string, b []byte) error {
	return os.WriteFile(path, b, 0o644)
}

// asker reads answers from stdin, one line each, so ktms init also takes them
// piped; secrets are read without echo on a terminal.
type asker struct {
	r   *bufio.Reader
	fd  int // -1 unless stdin is a terminal
	out io.Writer
}

func (a *app) newAsker() *asker {
	k := &asker{r: bufio.NewReader(a.stdin), fd: -1, out: a.stderr}
	if f, ok := a.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		k.fd = int(f.Fd())
	}
	return k
}

func (k *asker) read() (string, error) {
	s, err := k.r.ReadString('\n')
	if err != nil && s == "" {
		return "", usageError{errors.New("no answer on stdin")}
	}
	return strings.TrimSpace(s), nil
}

func (k *asker) line(prompt, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(k.out, "%s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(k.out, "%s: ", prompt)
	}
	s, err := k.read()
	if s == "" {
		return def, err
	}
	return s, err
}

func (k *asker) yes(prompt string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	fmt.Fprintf(k.out, "%s [%s] ", prompt, hint)
	s, err := k.read()
	if err != nil {
		return false, err
	}
	switch strings.ToLower(s) {
	case "":
		return def, nil
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	}
	return false, usageError{fmt.Errorf("%q: answer y or n", s)}
}

func (k *asker) secret(prompt string) (string, error) {
	fmt.Fprint(k.out, prompt+": ")
	if k.fd < 0 {
		return k.read()
	}
	b, err := term.ReadPassword(k.fd)
	fmt.Fprintln(k.out)
	return strings.TrimSpace(string(b)), err
}
