package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ai-process/ktms/internal/nodeagent"
)

func TestOnlyModelFlag(t *testing.T) {
	s := newSettings("test")
	if err := s.fs.Parse([]string{"--only-model", "a:1b, b:2b", "--only-model=c:3b", "--only-model", ","}); err != nil {
		t.Fatal(err)
	}
	want := []string{"a:1b", "b:2b", "c:3b"}
	if !reflect.DeepEqual([]string(s.onlyModels), want) {
		t.Errorf("only-model = %v, want %v", s.onlyModels, want)
	}
}

func TestScheduleCommand(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(good, []byte("00:00-24:00 50\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("10:00-11:00 50\n10:30-12:00 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args        []string
		code        int
		out, errOut string
	}{
		{[]string{"--file", good}, 0, "now 50%, no switches", ""},
		{[]string{"--file", good, "--quiet"}, 0, "", ""},
		{[]string{"--file", bad}, 1, "", "line 2 overlaps line 1"},
		{[]string{"--file", filepath.Join(dir, "missing")}, 1, "", "no such file"},
		{nil, 2, "", "--file is required"},
	} {
		var out, errOut bytes.Buffer
		code := runSchedule(c.args, &out, &errOut)
		if code != c.code || !strings.Contains(out.String(), c.out) || !strings.Contains(errOut.String(), c.errOut) || (c.out == "" && out.Len() > 0) {
			t.Errorf("schedule %v: code %d, stdout %q, stderr %q", c.args, code, out.String(), errOut.String())
		}
	}
}

func TestStatsCommand(t *testing.T) {
	dir := t.TempDir()
	st, err := nodeagent.OpenStats(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour)
	if err := st.AppendRequests([]nodeagent.RequestRecord{{RequestID: "a", StartedAt: at, DurationMs: 2000, Model: "tiny:1b", Outcome: "ok", InputTokens: 7}}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args        []string
		code        int
		out, errOut string
	}{
		{[]string{"--dir", dir, "--format", "json"}, 0, `"input_tokens": 7`, ""},
		{[]string{"--dir", dir, "--by", "model"}, 0, "tiny:1b", ""},
		{[]string{"--dir", filepath.Join(dir, "none")}, 0, "No usage log", ""},
		{[]string{"--dir", dir, "--since", "yesterday"}, 2, "", "bad time"},
		{[]string{"--dir", dir, "--since", "1h", "--until", "2h"}, 2, "", "--until must be after --since"},
		{[]string{"--dir", dir, "--format", "xml"}, 2, "", "unknown --format"},
	} {
		var out, errOut bytes.Buffer
		code := runStats(c.args, &out, &errOut)
		if code != c.code || !strings.Contains(out.String(), c.out) || !strings.Contains(errOut.String(), c.errOut) {
			t.Errorf("stats %v: code %d, stdout %q, stderr %q", c.args, code, out.String(), errOut.String())
		}
	}
}
