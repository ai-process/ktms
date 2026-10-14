package nodeagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A probe still retrying a dead run's address must not announce it after the
// run is gone, or the agent would register the dead address last.
func TestTunnelNeverAnnouncesDeadRunsURL(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
n=$(cat "` + dir + `/runs" 2>/dev/null || echo 0)
n=$((n+1))
echo $n > "` + dir + `/runs"
if [ "$n" = "1" ]; then
	echo "https://aaa-stale.trycloudflare.com" >&2
	sleep 1
	exit 1
fi
echo "https://bbb-fresh.trycloudflare.com" >&2
exec sleep 300
`
	bin := filepath.Join(dir, "cloudflared")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tr := &tunnelRunner{
		binary: bin,
		giveUp: 1200 * time.Millisecond,
		probe: func(_ context.Context, url string) error {
			if strings.Contains(url, "aaa") {
				return errors.New("dns not ready")
			}
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan string, 8)
	go tr.run(ctx, "127.0.0.1:1", out)

	var got []string
	deadline := time.After(4 * time.Second)
loop:
	for {
		select {
		case u := <-out:
			got = append(got, u)
		case <-deadline:
			break loop
		}
	}
	if len(got) != 1 || !strings.Contains(got[0], "bbb-fresh") {
		t.Fatalf("announced %v, want only the live run's address", got)
	}
}
