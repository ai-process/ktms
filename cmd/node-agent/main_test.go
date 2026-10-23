package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
