package nodeagent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

var quickTunnelURL = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// tunnelRunner keeps a cloudflared quick tunnel alive in front of the gateway
// and reports every public URL it obtains. The URL changes on each restart,
// which is why the agent re-registers when one arrives.
type tunnelRunner struct {
	binary string
	probe  func(ctx context.Context, url string) error // proves the URL reaches the gateway
	giveUp time.Duration                               // how long announce probes before sending anyway; 0 = 90s
}

func (t *tunnelRunner) run(ctx context.Context, localAddr string, out chan<- string) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		tail, err := t.once(ctx, localAddr, out)
		if ctx.Err() != nil {
			return
		}
		log.Warn().Err(err).Str("last_output", tail).Dur("retry_in", backoff).Msg("cloudflared exited, restarting tunnel")
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// once runs cloudflared until it exits and returns its last output lines.
func (t *tunnelRunner) once(ctx context.Context, localAddr string, out chan<- string) (string, error) {
	cmd := exec.CommandContext(ctx, t.binary, "tunnel", "--no-autoupdate", "--url", "http://"+localAddr)
	cmd.WaitDelay = 5 * time.Second
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start %s: %w", t.binary, err)
	}

	// Each run announces only while it is alive: a slow probe of a dead run's URL
	// must not be sent after the next run's URL.
	runCtx, endRun := context.WithCancel(ctx)
	defer endRun()

	var tail []string
	announced := false
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			line := sc.Text()
			log.Debug().Str("cloudflared", line).Msg("tunnel output")
			if tail = append(tail, line); len(tail) > 4 {
				tail = tail[1:]
			}
			if u := quickTunnelURL.FindString(line); u != "" && !announced {
				announced = true
				go t.announce(runCtx, u, out)
			}
		}
	}()

	err := cmd.Wait()
	_ = pw.Close()
	<-scanDone
	return strings.Join(tail, " | "), err
}

// announce waits until the tunnel answers end to end, so the proxy never gets
// an address whose DNS has not propagated yet. On timeout it sends it anyway.
func (t *tunnelRunner) announce(ctx context.Context, url string, out chan<- string) {
	giveUp := t.giveUp
	if giveUp == 0 {
		giveUp = 90 * time.Second
	}
	deadline := time.Now().Add(giveUp)
	for t.probe != nil {
		err := t.probe(ctx, url)
		if err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			log.Warn().Err(err).Str("url", url).Msg("tunnel not reachable yet, announcing anyway")
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	if ctx.Err() != nil {
		return
	}
	select {
	case out <- url:
	case <-ctx.Done():
	}
}

func httpProbe(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz status %d", resp.StatusCode)
	}
	return nil
}
