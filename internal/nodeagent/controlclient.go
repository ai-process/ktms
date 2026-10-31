package nodeagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrAgentDown means nothing answers on the control socket.
var ErrAgentDown = errors.New("the node agent is not running (or predates the control API)")

// ControlCallError is a refusal from the agent.
type ControlCallError struct {
	Status int
	ControlError
}

func (e *ControlCallError) Error() string { return e.ControlError.Error }

// ControlClient talks to a running agent over its control socket; the CLI
// and the tray both use it.
type ControlClient struct {
	Socket string
	hc     *http.Client
}

func NewControlClient(socket string) *ControlClient {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}
	return &ControlClient{Socket: socket, hc: &http.Client{Transport: &http.Transport{DialContext: dial}}}
}

func (c *ControlClient) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*ctlTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return c.dialErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		e := &ControlCallError{Status: resp.StatusCode}
		if err := json.NewDecoder(resp.Body).Decode(&e.ControlError); err != nil || e.ControlError.Error == "" {
			e.ControlError.Error = fmt.Sprintf("agent answered %s", resp.Status)
		}
		return e
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *ControlClient) dialErr(err error) error {
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("%w: no answer on %s", ErrAgentDown, c.Socket)
	}
	return err
}

func (c *ControlClient) Status(ctx context.Context) (*Status, error) {
	var s Status
	return &s, c.call(ctx, http.MethodGet, "/v1/status", nil, &s)
}

func (c *ControlClient) SetOverride(ctx context.Context, req OverrideRequest) (*OverrideResult, error) {
	var r OverrideResult
	return &r, c.call(ctx, http.MethodPost, "/v1/override", req, &r)
}

func (c *ControlClient) ClearOverride(ctx context.Context) (*Status, error) {
	var s Status
	return &s, c.call(ctx, http.MethodDelete, "/v1/override", nil, &s)
}

func (c *ControlClient) Schedule(ctx context.Context) (*ScheduleInfo, error) {
	var s ScheduleInfo
	return &s, c.call(ctx, http.MethodGet, "/v1/schedule", nil, &s)
}

func (c *ControlClient) PutSchedule(ctx context.Context, text string) (*ScheduleInfo, error) {
	var s ScheduleInfo
	return &s, c.call(ctx, http.MethodPut, "/v1/schedule", scheduleBody{Text: text}, &s)
}

func (c *ControlClient) PreviewSchedule(ctx context.Context, text string) (*ScheduleInfo, error) {
	var s ScheduleInfo
	return &s, c.call(ctx, http.MethodPost, "/v1/schedule/preview", scheduleBody{Text: text}, &s)
}

// Stats takes from and to in any form ParseWhen reads; by may be empty.
func (c *ControlClient) Stats(ctx context.Context, from, to, by string) (*Report, error) {
	q := url.Values{}
	for k, v := range map[string]string{"from": from, "to": to, "by": by} {
		if v != "" {
			q.Set(k, v)
		}
	}
	var r Report
	return &r, c.call(ctx, http.MethodGet, "/v1/stats?"+q.Encode(), nil, &r)
}

// Events calls fn with each status the agent streams until ctx ends or the
// stream breaks.
func (c *ControlClient) Events(ctx context.Context, fn func(*Status)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent/v1/events", nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return c.dialErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent answered %s", resp.Status)
	}
	// Silence longer than this means the agent is gone, though the socket is not.
	idle := time.AfterFunc(3*eventsEvery, func() { resp.Body.Close() })
	defer idle.Stop()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		idle.Reset(3 * eventsEvery)
		var s Status
		if err := json.Unmarshal([]byte(data), &s); err != nil {
			return err
		}
		fn(&s)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("the agent closed the event stream")
}
