package main

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ai-process/ktms/internal/nodeagent"
)

func TestReorderKeepsFlagValues(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("for", "", "")
	fs.Bool("now", false, "")
	got := reorder(fs, []string{"50", "--for", "1h", "--now", "x", "--for=2h"})
	want := []string{"--for", "1h", "--now", "--for=2h", "50", "x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reorder = %v, want %v", got, want)
	}
}

func TestControlCommandsWithoutAgent(t *testing.T) {
	dir, err := os.MkdirTemp("", "kn")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := "--control-socket=" + filepath.Join(dir, "none.sock")
	for _, c := range []struct {
		run  func([]string, io.Writer, io.Writer) int
		args []string
		code int
		err  string
	}{
		{runStatus, []string{sock}, 1, "ktms-node doctor"},
		{runPause, []string{sock, "5m", "--now"}, 1, "not running"},
		{runPause, []string{sock, "5m", "--until-next"}, 2, "not both"},
		{runLevel, []string{sock, "50"}, 2, "Usage"},
		{runLevel, []string{sock, "150", "--for", "1h"}, 2, "0 to 100"},
		{runLevel, []string{sock, "50%", "--for", "1h"}, 1, "not running"},
		{runResume, []string{sock}, 1, "not running"},
	} {
		var out, errOut bytes.Buffer
		if code := c.run(c.args, &out, &errOut); code != c.code || !strings.Contains(errOut.String(), c.err) {
			t.Errorf("%v: code %d, stderr %q; want %d with %q", c.args, code, errOut.String(), c.code, c.err)
		}
	}
}

func TestPrintStatus(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)
	s := &nodeagent.Status{
		NodeID: "mac", Proxy: "gw:443", State: nodeagent.StatePausing,
		Connection: nodeagent.StatusConn{Registered: true, LastHeartbeatAt: now.Add(-3 * time.Second)},
		Level: nodeagent.StatusLevel{Effective: 0, Source: nodeagent.SourceOverride,
			Override: &nodeagent.Override{Until: now.Add(10 * time.Minute)},
			Next:     &nodeagent.StatusNext{At: now.Add(10 * time.Minute), Level: 100}},
		Requests:        nodeagent.StatusRequests{ProxyActive: 2},
		CapacityApplied: true,
		Models:          []nodeagent.StatusModel{{ID: "q:32b", Warm: true}},
	}
	var b bytes.Buffer
	printStatus(&b, s, now)
	for _, want := range []string{"heartbeat 3s ago", "Level 0% (set by hand until 09:10, then 100% at 09:10)", "waiting for 2 proxy request(s)", "q:32b (loaded)"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, b.String())
		}
	}
}
