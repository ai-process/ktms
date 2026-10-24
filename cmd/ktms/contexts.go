package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Contexts is ~/.config/ktms/config.yaml: named Keystones and where each
// one's keys live. Keys themselves are never written to this file.
type Contexts struct {
	Current  string              `yaml:"current_context,omitempty" json:"current_context,omitempty"`
	Contexts map[string]*Context `yaml:"contexts,omitempty" json:"contexts,omitempty"`
}

// Context is one Keystone.
type Context struct {
	Addr      string    `yaml:"addr,omitempty" json:"addr,omitempty"` // Admin API host:port
	TLS       bool      `yaml:"tls,omitempty" json:"tls,omitempty"`
	HTTP      string    `yaml:"http,omitempty" json:"http,omitempty"`             // base URL of /v1 and the probes
	Key       KeySource `yaml:"key,omitempty" json:"key,omitempty"`               // admin or admin:read key
	ClientKey KeySource `yaml:"client_key,omitempty" json:"client_key,omitempty"` // generate key for ktms chat
}

// KeySource says where a key is read from; exactly one field is set.
type KeySource struct {
	Env      string `yaml:"env,omitempty" json:"env,omitempty"`
	File     string `yaml:"file,omitempty" json:"file,omitempty"`
	Keychain bool   `yaml:"keychain,omitempty" json:"keychain,omitempty"`
}

func (k KeySource) empty() bool { return k == KeySource{} }

func (k KeySource) String() string {
	switch {
	case k.Env != "":
		return "env:" + k.Env
	case k.File != "":
		return "file:" + k.File
	case k.Keychain:
		return "keychain"
	}
	return "-"
}

// parseKeySource reads env:VAR, file:PATH or keychain.
func parseKeySource(s string) (KeySource, error) {
	switch {
	case strings.HasPrefix(s, "env:") && len(s) > 4:
		return KeySource{Env: s[4:]}, nil
	case strings.HasPrefix(s, "file:") && len(s) > 5:
		return KeySource{File: s[5:]}, nil
	case s == "keychain":
		return KeySource{Keychain: true}, nil
	}
	return KeySource{}, fmt.Errorf("key source %q: want env:VAR, file:PATH or keychain", s)
}

// read returns the key; account names the keychain entry.
func (k KeySource) read(account string) (string, error) {
	switch {
	case k.Env != "":
		if v := os.Getenv(k.Env); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("%s is not set", k.Env)
	case k.File != "":
		return readKeyFile(k.File)
	case k.Keychain:
		return keychainGet(account)
	}
	return "", errors.New("no key source")
}

func readKeyFile(path string) (string, error) {
	b, err := os.ReadFile(expandHome(path))
	if err != nil {
		return "", err
	}
	k := strings.TrimSpace(string(b))
	if k == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return k, nil
}

func configDir() (string, error) {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "ktms"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "ktms"), nil
}

// contextsUnreachable is a contexts file with no place to be, as without $HOME.
type contextsUnreachable struct{ error }

func contextsPath() (string, error) {
	if p := os.Getenv("KTMS_CONFIG"); p != "" {
		return p, nil
	}
	d, err := configDir()
	if err != nil {
		return "", contextsUnreachable{err}
	}
	return filepath.Join(d, "config.yaml"), nil
}

// loadContexts returns an empty set when the file does not exist yet.
func loadContexts() (*Contexts, error) {
	p, err := contextsPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &Contexts{Contexts: map[string]*Context{}}, nil
	}
	if err != nil {
		return nil, err
	}
	c := &Contexts{}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if c.Contexts == nil {
		c.Contexts = map[string]*Context{}
	}
	return c, nil
}

func (c *Contexts) save() error {
	p, err := contextsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return writePrivate(p, b)
}

func (c *Contexts) names() []string {
	out := make([]string, 0, len(c.Contexts))
	for n := range c.Contexts {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// writePrivate writes a file only its owner can read, replacing it whole.
func writePrivate(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// The system keychain through its own tools: security on macOS, secret-tool
// (libsecret) on Linux. The key goes over stdin, never on a command line.
const keychainService = "ktms"

func keychainGet(account string) (string, error) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("security", "find-generic-password", "-s", keychainService, "-a", account, "-w")
	case "linux":
		cmd = exec.Command("secret-tool", "lookup", "service", keychainService, "account", account)
	default:
		return "", fmt.Errorf("no keychain support on %s", runtime.GOOS)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("keychain entry %s/%s: %v %s", keychainService, account, err, strings.TrimSpace(errb.String()))
	}
	k := strings.TrimSpace(out.String())
	if k == "" {
		return "", fmt.Errorf("keychain entry %s/%s is empty", keychainService, account)
	}
	return k, nil
}

func keychainSet(account, key string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		// security -i reads commands from stdin, keeping the key out of ps.
		cmd = exec.Command("security", "-i")
		cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n",
			keychainService, quoteSecurity(account), quoteSecurity(key)))
	case "linux":
		cmd = exec.Command("secret-tool", "store", "--label=ktms "+account, "service", keychainService, "account", account)
		cmd.Stdin = strings.NewReader(key)
	default:
		return fmt.Errorf("no keychain support on %s", runtime.GOOS)
	}
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &errb, &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("keychain: %v %s", err, strings.TrimSpace(errb.String()))
	}
	return nil
}

func quoteSecurity(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
