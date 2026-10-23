package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// printer writes a result as a table for people or as JSON or YAML for
// scripts. JSON and YAML carry the API messages as protojson renders them.
type printer struct {
	w      io.Writer
	format string // table | json | yaml
}

func newPrinter(w io.Writer, format string) (*printer, error) {
	switch format {
	case "", "table":
		return &printer{w: w, format: "table"}, nil
	case "json", "yaml":
		return &printer{w: w, format: format}, nil
	}
	return nil, usageError{fmt.Errorf("-o %q: want table, json or yaml", format)}
}

func (p *printer) table() bool { return p.format == "table" }

// data writes v, which may hold proto messages, as JSON or YAML.
func (p *printer) data(v any) error {
	b, err := toJSON(v)
	if err != nil {
		return err
	}
	if p.format == "json" {
		var out bytes.Buffer
		if err := json.Indent(&out, b, "", "  "); err != nil {
			return err
		}
		out.WriteByte('\n')
		_, err := p.w.Write(out.Bytes())
		return err
	}
	// JSON is YAML; re-encoding the node keeps the key order.
	var n yaml.Node
	if err := yaml.Unmarshal(b, &n); err != nil {
		return err
	}
	blockStyle(&n)
	enc := yaml.NewEncoder(p.w)
	enc.SetIndent(2)
	if err := enc.Encode(&n); err != nil {
		return err
	}
	return enc.Close()
}

// blockStyle drops the flow and quoting styles JSON came with; the encoder
// still quotes a string that would read back as another type.
func blockStyle(n *yaml.Node) {
	n.Style = 0
	for _, c := range n.Content {
		blockStyle(c)
	}
}

// toJSON marshals proto messages with protojson and everything else with
// encoding/json, nesting freely.
func toJSON(v any) ([]byte, error) {
	switch x := v.(type) {
	case proto.Message:
		return protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}.Marshal(x)
	case []any:
		parts := make([]json.RawMessage, len(x))
		for i, e := range x {
			b, err := toJSON(e)
			if err != nil {
				return nil, err
			}
			parts[i] = b
		}
		return json.Marshal(parts)
	case map[string]any:
		obj := make(map[string]json.RawMessage, len(x))
		for k, e := range x {
			b, err := toJSON(e)
			if err != nil {
				return nil, err
			}
			obj[k] = b
		}
		return json.Marshal(obj)
	}
	return json.Marshal(v)
}

func messages[T proto.Message](ms []T) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}

// rows writes a table with a header line.
func (p *printer) rows(header []string, rows [][]string) error {
	tw := tabwriter.NewWriter(p.w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	return tw.Flush()
}

func usd(nano int64) string {
	return fmt.Sprintf("$%.4f", float64(nano)/1e9)
}

func when(ts *timestamppb.Timestamp) string {
	if ts == nil || (ts.GetSeconds() == 0 && ts.GetNanos() == 0) {
		return "-"
	}
	return ts.AsTime().UTC().Format(time.RFC3339)
}

func list(vs []string) string {
	if len(vs) == 0 {
		return "-"
	}
	return strings.Join(vs, ",")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
