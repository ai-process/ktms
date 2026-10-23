package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

// route is what Keystone reports about how it served a call.
var routeHeaders = []string{"Rule", "Model", "Vendor", "Route-Mode", "Placement", "Judge-Model", "Judge-Score", "Cache"}

func (a *app) chatCmd() *cobra.Command {
	var model, effort, system, user, keyFile, format string
	var attrs map[string]string
	var maxTokens int
	cmd := &cobra.Command{
		Use:   "chat [PROMPT]",
		Short: "Send one test call through /v1/chat/completions and show how it was routed",
		Long: `Send one prompt (an argument, or stdin) through the OpenAI-compatible API with
a client key (scope generate) from --client-key-file, KTMS_API_KEY or the
context's client_key, then print the answer and the rule, model and vendor
that served it.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			prompt := ""
			if len(args) == 1 {
				prompt = args[0]
			} else {
				b, err := io.ReadAll(a.stdin)
				if err != nil {
					return err
				}
				prompt = strings.TrimSpace(string(b))
			}
			if prompt == "" {
				return usageError{errors.New("no prompt: pass it as an argument or on stdin")}
			}
			key, err := a.t.clientKey(keyFile)
			if err != nil {
				return err
			}
			base, err := a.t.httpBase()
			if err != nil {
				return err
			}
			var msgs []map[string]string
			if system != "" {
				msgs = append(msgs, map[string]string{"role": "system", "content": system})
			}
			msgs = append(msgs, map[string]string{"role": "user", "content": prompt})
			body := map[string]any{"model": model, "messages": msgs}
			if effort != "" {
				body["reasoning_effort"] = effort
			}
			if user != "" {
				body["user"] = user
			}
			if len(attrs) > 0 {
				body["metadata"] = attrs
			}
			if maxTokens > 0 {
				body["max_tokens"] = maxTokens
			}
			b, _ := json.Marshal(body)
			ctx, cancel := a.ctx()
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(b))
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			res, err := a.t.httpClient().Do(req)
			if err != nil {
				return err
			}
			defer res.Body.Close()
			raw, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
			if err != nil {
				return err
			}
			if res.StatusCode/100 != 2 {
				return httpError{status: res.StatusCode, body: apiMessage(raw)}
			}

			var reply struct {
				Model   string `json:"model"`
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
				Usage map[string]any `json:"usage"`
			}
			if err := json.Unmarshal(raw, &reply); err != nil {
				return fmt.Errorf("unreadable answer: %w", err)
			}
			route := map[string]string{}
			for _, h := range routeHeaders {
				if v := res.Header.Get("X-KTMS-" + h); v != "" {
					route[routeKey(h)] = v
				}
			}
			if route["model"] == "" && reply.Model != "" {
				route["model"] = reply.Model // the body names the model that answered
			}
			if !p.table() {
				return p.data(map[string]any{"response": json.RawMessage(raw), "route": route})
			}
			if len(reply.Choices) > 0 {
				fmt.Fprintln(a.stdout, reply.Choices[0].Message.Content)
			}
			var parts []string
			for _, h := range routeHeaders {
				if v := route[routeKey(h)]; v != "" {
					parts = append(parts, strings.ToLower(h)+": "+v)
				}
			}
			if u := reply.Usage; u != nil {
				parts = append(parts, fmt.Sprintf("tokens: %v in, %v out", u["prompt_tokens"], u["completion_tokens"]))
			}
			fmt.Fprintln(a.stderr, "—", strings.Join(parts, ", "))
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&model, "model", "auto", "model id, or auto to let the rules choose")
	f.StringVar(&effort, "effort", "", "low, medium or high")
	f.StringVar(&system, "system", "", "system prompt")
	f.StringVar(&user, "user", "", "end-user id (user_id for limits and rules)")
	f.StringToStringVar(&attrs, "attr", nil, "request attribute key=value, matched by rules")
	f.IntVar(&maxTokens, "max-tokens", 0, "cap on output tokens")
	f.StringVar(&keyFile, "client-key-file", "", "read the client key from this file instead of KTMS_API_KEY")
	_ = cmd.RegisterFlagCompletionFunc("effort", cobra.FixedCompletions([]string{"low", "medium", "high"}, cobra.ShellCompDirectiveNoFileComp))
	outputFlag(cmd, &format)
	return cmd
}

func routeKey(header string) string {
	return strings.ToLower(strings.ReplaceAll(header, "-", "_"))
}

// apiMessage pulls error.message out of an OpenAI-style error body.
func apiMessage(raw []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func (a *app) healthCmd() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Check /healthz and /readyz and show Keystone's version",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			base, err := a.t.httpBase()
			if err != nil {
				return err
			}
			ctx, cancel := a.ctx()
			defer cancel()
			type probe struct {
				Path    string         `json:"path"`
				Status  int            `json:"status"`
				Body    map[string]any `json:"body,omitempty"`
				Problem string         `json:"error,omitempty"`
			}
			var probes []probe
			ready := true
			for _, path := range []string{"/healthz", "/readyz"} {
				pr := probe{Path: path}
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
				if err != nil {
					return err
				}
				res, err := a.t.httpClient().Do(req)
				if err != nil {
					pr.Problem, ready = err.Error(), false
				} else {
					raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
					res.Body.Close()
					pr.Status = res.StatusCode
					_ = json.Unmarshal(raw, &pr.Body)
					ready = ready && res.StatusCode == http.StatusOK
				}
				probes = append(probes, pr)
			}
			if !p.table() {
				if err := p.data(map[string]any{"http": base, "probes": probes}); err != nil {
					return err
				}
			} else {
				var rows [][]string
				for _, pr := range probes {
					st, detail := strings.TrimSpace(fmt.Sprint(pr.Status)), pr.Problem
					if pr.Problem == "" {
						detail = fmt.Sprint(pr.Body["status"])
						if v, ok := pr.Body["version"]; ok {
							detail += ", version " + fmt.Sprint(v)
						}
						if d, ok := pr.Body["database"]; ok {
							detail += ", database " + fmt.Sprint(d)
						}
					} else {
						st = "-"
					}
					rows = append(rows, []string{base + pr.Path, st, detail})
				}
				if err := p.rows([]string{"PROBE", "STATUS", "DETAIL"}, rows); err != nil {
					return err
				}
			}
			if !ready {
				return codeError{exitRetry}
			}
			return nil
		},
	}
	outputFlag(cmd, &format)
	return cmd
}
