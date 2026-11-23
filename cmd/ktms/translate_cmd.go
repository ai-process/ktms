package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// One request's worth of lines, under the server's 128 texts and 256 KiB.
const (
	translateBatchTexts = 128
	translateBatchBytes = 200 << 10
)

type translateLine struct {
	Text     string `json:"text"`
	Result   string `json:"translation"`
	Detected string `json:"detected_source_language,omitempty"`
	Model    string `json:"model,omitempty"`
}

func (a *app) translateCmd() *cobra.Command {
	var from, to, format, model, keyFile, output, action string
	var attrs map[string]string
	cmd := &cobra.Command{
		Use:   "translate --to LANG [--from LANG] [FILE|-]",
		Short: "Translate text line by line through /v1/translate",
		Long: `Translate each line of FILE, or of stdin, into --to through the translate rule's
chain (a node MT model, a chat model by prompt, or a cloud MT), with a client
key (scope generate) from --client-key-file, KTMS_API_KEY or the context's
client_key. Prints one translation per line; empty lines stay empty. Without
--from each line's language is detected. -o json also prints the model that
served each line and the detected language.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := newPrinter(a.stdout, output)
			if err != nil {
				return err
			}
			if to == "" {
				return usageError{errors.New("--to is required, e.g. --to de")}
			}
			in := a.stdin
			if len(args) == 1 && args[0] != "-" {
				f, err := os.Open(args[0])
				if err != nil {
					return err
				}
				defer f.Close()
				in = f
			}
			lines, err := readLines(in)
			if err != nil {
				return err
			}
			key, err := a.t.clientKey(keyFile)
			if err != nil {
				return err
			}
			base, err := a.t.httpBase()
			if err != nil {
				return err
			}
			out := make([]translateLine, len(lines))
			var batch []int
			size := 0
			flush := func() error {
				if len(batch) == 0 {
					return nil
				}
				texts := make([]string, len(batch))
				for i, idx := range batch {
					texts[i] = lines[idx]
				}
				res, err := a.translateBatch(base, key, map[string]any{
					"texts": texts, "source_language": from, "target_language": to, "format": format,
					"model_override": model, "attributes": attrs, "action_id": action,
				})
				if err != nil {
					return err
				}
				if len(res.Translations) != len(batch) {
					return fmt.Errorf("server returned %d translations for %d lines", len(res.Translations), len(batch))
				}
				for i, idx := range batch {
					out[idx].Result = res.Translations[i].Text
					out[idx].Detected = res.Translations[i].DetectedSourceLanguage
					out[idx].Model = res.ResolvedModel
				}
				batch, size = batch[:0], 0
				return nil
			}
			// Checked before the first call, so a long line fails fast instead of
			// after part of the file was translated and paid for.
			for i, l := range lines {
				if len(l) > translateBatchBytes {
					return usageError{fmt.Errorf("line %d is %d KiB; split it, at most %d KiB per line", i+1, len(l)>>10, translateBatchBytes>>10)}
				}
			}
			for i, l := range lines {
				out[i].Text = l
				if strings.TrimSpace(l) == "" {
					continue
				}
				if len(batch) == translateBatchTexts || size+len(l) > translateBatchBytes {
					if err := flush(); err != nil {
						return err
					}
				}
				batch = append(batch, i)
				size += len(l)
			}
			if err := flush(); err != nil {
				return err
			}
			if !p.table() {
				return p.data(out)
			}
			w := bufio.NewWriter(a.stdout)
			for _, l := range out {
				// One line in, one line out, so the output lines up with the input.
				fmt.Fprintln(w, lineBreaks.Replace(l.Result))
			}
			return w.Flush()
		},
	}
	f := cmd.Flags()
	f.StringVar(&to, "to", "", "target language, BCP-47 (de, pt-BR)")
	f.StringVar(&from, "from", "", "source language; detected per line when empty")
	f.StringVar(&format, "format", "text", "text or html")
	f.StringVar(&model, "model", "", "a model override instead of the rules")
	f.StringVar(&action, "action", "", "action_id in usage (default translate)")
	f.StringToStringVar(&attrs, "attr", nil, "request attribute key=value, matched by rules")
	f.StringVar(&keyFile, "client-key-file", "", "read the client key from this file instead of KTMS_API_KEY")
	outputFlag(cmd, &output)
	return cmd
}

// lineBreaks keeps one translation on one output line; a bare \r would send
// the terminal back to column 0.
var lineBreaks = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

type translateReply struct {
	Translations []struct {
		Text                   string `json:"text"`
		DetectedSourceLanguage string `json:"detected_source_language"`
	} `json:"translations"`
	ResolvedModel string `json:"resolved_model"`
}

func (a *app) translateBatch(base, key string, body map[string]any) (*translateReply, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	ctx, cancel := a.ctx()
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/translate", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	res, err := a.t.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		return nil, httpError{status: res.StatusCode, body: apiMessage(raw)}
	}
	var reply translateReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("unreadable answer: %w", err)
	}
	return &reply, nil
}

func readLines(r io.Reader) ([]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}
