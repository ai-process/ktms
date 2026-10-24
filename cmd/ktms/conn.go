package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

// target is where and how to reach Keystone. Flags win over environment
// variables, which win over the selected context.
type target struct {
	context string
	addr    string
	tls     bool
	tlsSet  bool
	http    string
	keyFile string

	ctx      *Context // the selected context, nil without one
	ctxName  string
	warn     io.Writer
	resolved bool
	httpOnce sync.Once
	hc       *http.Client
	// KTMS_ADMIN_KEY and KTMS_API_KEY, unless a context was asked for by name.
	envAdminKey, envClientKey string
}

// resolve fills addr, tls and http from the environment and the context, on
// first use, so commands that need no Keystone never read the contexts file.
func (t *target) resolve() error {
	if t.resolved {
		return nil
	}
	t.resolved = true
	asked := firstNonEmpty(t.context, os.Getenv("KTMS_CONTEXT"))
	cs, err := loadContexts()
	var noHome contextsUnreachable
	if errors.As(err, &noHome) && asked == "" {
		// No home directory (cron, systemd): flags and environment still work.
		cs, err = &Contexts{}, nil
	}
	if err != nil {
		return err
	}
	name := firstNonEmpty(asked, cs.Current)
	if name != "" {
		c, ok := cs.Contexts[name]
		if !ok {
			return fmt.Errorf("no context %q (ktms context list)", name)
		}
		t.ctx, t.ctxName = c, name
	}
	c := t.ctx
	if c == nil {
		c = &Context{}
	}
	// A context asked for by name wins over the environment, so its keys never
	// go to an address left in the shell, nor the shell's keys to its address.
	env := os.Getenv
	if asked != "" {
		env = func(string) string { return "" }
	}
	t.envAdminKey, t.envClientKey = env("KTMS_ADMIN_KEY"), env("KTMS_API_KEY")
	t.addr = firstNonEmpty(t.addr, env("KTMS_ADDR"), c.Addr, "localhost:9090")
	if !t.tlsSet {
		if v := env("KTMS_TLS"); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return usageError{fmt.Errorf("KTMS_TLS=%q: want true or false", v)}
			}
			t.tls = b
		} else {
			t.tls = c.TLS
		}
	}
	t.http = firstNonEmpty(t.http, env("KTMS_HTTP"), c.HTTP)
	if t.http == "" {
		host, _, err := net.SplitHostPort(t.addr)
		if err != nil {
			host = t.addr
		}
		scheme := "http"
		if t.tls {
			scheme = "https"
		}
		t.http = scheme + "://" + net.JoinHostPort(host, "8080")
	}
	t.http = strings.TrimRight(t.http, "/")
	return nil
}

// adminKey reads the key from --key-file, KTMS_ADMIN_KEY or the context, never
// from a flag value, so it stays out of shell history and process listings.
func (t *target) adminKey() (string, error) {
	if err := t.resolve(); err != nil {
		return "", err
	}
	if t.keyFile != "" {
		return readKeyFile(t.keyFile)
	}
	if t.envAdminKey != "" {
		return t.envAdminKey, nil
	}
	if t.ctx != nil && !t.ctx.Key.empty() {
		k, err := t.ctx.Key.read(t.ctxName)
		if err != nil {
			return "", fmt.Errorf("admin key of context %s: %w", t.ctxName, err)
		}
		return k, nil
	}
	return "", authError{errors.New("no admin key: set KTMS_ADMIN_KEY, pass --key-file or add a context (ktms context set)")}
}

// clientKey is the generate key ktms chat calls /v1 with.
func (t *target) clientKey(file string) (string, error) {
	if err := t.resolve(); err != nil {
		return "", err
	}
	if file != "" {
		return readKeyFile(file)
	}
	if t.envClientKey != "" {
		return t.envClientKey, nil
	}
	if t.ctx != nil && !t.ctx.ClientKey.empty() {
		k, err := t.ctx.ClientKey.read(t.ctxName + "/client")
		if err != nil {
			return "", fmt.Errorf("client key of context %s: %w", t.ctxName, err)
		}
		return k, nil
	}
	return "", authError{errors.New("no client key: set KTMS_API_KEY, pass --client-key-file or set the context's client_key")}
}

type bearer struct {
	key    string
	secure bool
}

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.key}, nil
}
func (b bearer) RequireTransportSecurity() bool { return b.secure }

func (t *target) admin() (pb.AdminServiceClient, func(), error) {
	key, err := t.adminKey()
	if err != nil {
		return nil, nil, err
	}
	if !t.tls && !loopback(t.addr) {
		fmt.Fprintf(t.warn, "ktms: warning: sending the admin key to %s without TLS; pass --tls unless this network is private\n", t.addr)
	}
	creds := insecure.NewCredentials()
	if t.tls {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	conn, err := grpc.NewClient(t.addr, grpc.WithTransportCredentials(creds), grpc.WithPerRPCCredentials(bearer{key: key, secure: t.tls}))
	if err != nil {
		return nil, nil, err
	}
	return pb.NewAdminServiceClient(conn), func() { _ = conn.Close() }, nil
}

// httpBase is the HTTP API's base URL.
func (t *target) httpBase() (string, error) {
	if err := t.resolve(); err != nil {
		return "", err
	}
	return t.http, nil
}

// httpClient is shared: ktms mcp probes health for as long as it runs.
func (t *target) httpClient() *http.Client {
	t.httpOnce.Do(func() {
		t.hc = &http.Client{Transport: &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			IdleConnTimeout: 90 * time.Second,
		}}
	})
	return t.hc
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

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
