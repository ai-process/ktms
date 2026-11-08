package agentrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type reply struct{ id, text, status string }

// fakeInbox answers like a server that ignores since and wait (before 1.6)
// unless waits is set, and then like one that honours both.
type fakeInbox struct {
	mu      sync.Mutex
	tasks   []Task
	replies []reply
	waits   bool
	calls   int
	clock   time.Time
}

func (f *fakeInbox) Inbox(ctx context.Context, since time.Time, wait time.Duration) ([]Task, error) {
	f.mu.Lock()
	f.calls++
	if !f.waits {
		defer f.mu.Unlock()
		return append([]Task(nil), f.tasks...), nil
	}
	var out []Task
	for _, t := range f.tasks {
		if t.Updated.After(since) {
			out = append(out, t)
		}
	}
	f.mu.Unlock()
	if len(out) == 0 {
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
	return out, nil
}

func (f *fakeInbox) tick() time.Time {
	if f.clock.IsZero() {
		f.clock = time.Now()
	}
	f.clock = f.clock.Add(time.Millisecond)
	return f.clock
}

func (f *fakeInbox) Reply(_ context.Context, id, text, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, reply{id, text, status})
	for i := range f.tasks {
		if f.tasks[i].TaskID == id {
			f.tasks[i].Status = status
			f.tasks[i].Updated = f.tick()
			f.tasks[i].Messages = append(f.tasks[i].Messages, Message{From: "me", Role: "agent", Text: text})
		}
	}
	return nil
}

type fakeRunner struct {
	out     string
	err     error
	wait    time.Duration
	prompts []string
}

func (r *fakeRunner) Run(ctx context.Context, prompt string) (string, error) {
	r.prompts = append(r.prompts, prompt)
	select {
	case <-time.After(r.wait):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return r.out, r.err
}

func task(id, status string, msgs ...Message) Task {
	return Task{TaskID: id, From: "alice", To: "bob", Status: status, Messages: msgs}
}

func TestDue(t *testing.T) {
	user := Message{Role: "user", Text: "review PR 12"}
	cases := map[string]struct {
		t    Task
		want bool
	}{
		"new":               {task("1", "submitted", user), true},
		"answered question": {task("1", "working", user, Message{Role: "agent", Text: "which repo?"}, Message{Role: "user", Text: "ktms"}), true},
		"asked":             {task("1", "input-required", user, Message{Role: "agent", Text: "which repo?"}), false},
		"held":              {Task{Status: "input-required", Reason: "guardrail: injection", Messages: []Message{user}}, false},
		"progress note":     {task("1", "working", user, Message{Role: "agent", Text: "halfway"}), false},
		"claimed, died":     {task("1", "working", user, Message{Role: "agent", Text: Claim}), true},
		"ended":             {task("1", "completed", user), false},
	}
	for name, c := range cases {
		if got := Due(c.t); got != c.want {
			t.Errorf("%s: Due = %v, want %v", name, got, c.want)
		}
	}
}

func TestParse(t *testing.T) {
	cases := []struct{ in, text, status string }{
		{"LGTM\n\nSTATUS: completed\n", "LGTM", "completed"},
		{"Which repo?\n**Status: input-required**", "Which repo?", "input-required"},
		{"cannot\nstatus: FAILED", "cannot", "failed"},
		{"no status line", "no status line", "completed"},
		{"STATUS: completed\nbut more after", "STATUS: completed\nbut more after", "completed"},
	}
	for _, c := range cases {
		if text, status := Parse(c.in); text != c.text || status != c.status {
			t.Errorf("Parse(%q) = %q, %q; want %q, %q", c.in, text, status, c.text, c.status)
		}
	}
}

func TestPromptKeepsTaskAndNewest(t *testing.T) {
	big := strings.Repeat("x", 400)
	tk := task("1", "working", Message{Role: "user", Text: "THE TASK"},
		Message{Role: "agent", Text: "old " + big}, Message{Role: "user", Text: "mid " + big}, Message{Role: "user", Text: "NEWEST"})
	p := Prompt(tk, "do it", 900)
	if len(p) > 900 || !strings.Contains(p, "THE TASK") || !strings.Contains(p, "NEWEST") || strings.Contains(p, "old x") {
		t.Fatalf("prompt (%d bytes):\n%s", len(p), p)
	}
	if !strings.Contains(p, "older messages left out") {
		t.Fatal("the prompt does not say messages were left out")
	}
}

func TestOnce(t *testing.T) {
	ctx := context.Background()
	in := &fakeInbox{tasks: []Task{
		task("new", "submitted", Message{Role: "user", Text: "second"}),
		task("asked", "input-required", Message{Role: "user", Text: "x"}, Message{Role: "agent", Text: "q?"}),
		task("old", "submitted", Message{Role: "user", Text: "first"}),
	}}
	r := &fakeRunner{out: "Which branch?\nSTATUS: input-required"}
	if err := Once(ctx, in, r, Config{}); err != nil {
		t.Fatal(err)
	}
	want := []reply{{"old", Claim, "working"}, {"old", "Which branch?", "input-required"}, {"new", Claim, "working"}, {"new", "Which branch?", "input-required"}}
	if len(in.replies) != len(want) {
		t.Fatalf("replies = %+v", in.replies)
	}
	for i := range want {
		if in.replies[i] != want[i] {
			t.Fatalf("reply %d = %+v, want %+v", i, in.replies[i], want[i])
		}
	}
	if !strings.Contains(r.prompts[0], "first") || !strings.Contains(r.prompts[0], "STATUS: completed") {
		t.Fatalf("prompt = %s", r.prompts[0])
	}
	// Nothing is due any more: a second pass runs nothing.
	if err := Once(ctx, in, r, Config{}); err != nil || len(r.prompts) != 2 {
		t.Fatalf("second pass ran %d prompts, %v", len(r.prompts), err)
	}
}

func TestOnceFailures(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		r    *fakeRunner
		cfg  Config
		want string
	}{
		"error":   {&fakeRunner{out: "/home/me/secret path", err: errors.New("exit status 2")}, Config{}, "failed to run"},
		"timeout": {&fakeRunner{wait: time.Second}, Config{Timeout: 20 * time.Millisecond}, "did not finish"},
		"silent":  {&fakeRunner{out: "\n"}, Config{}, "no answer"},
	}
	for name, c := range cases {
		in := &fakeInbox{tasks: []Task{task("t", "submitted", Message{Role: "user", Text: "x"})}}
		if err := Once(ctx, in, c.r, c.cfg); err != nil {
			t.Fatal(err)
		}
		last := in.replies[len(in.replies)-1]
		if last.status != "failed" || !strings.Contains(last.text, c.want) || strings.Contains(last.text, "secret") {
			t.Errorf("%s: reply = %+v", name, last)
		}
	}

	in := &fakeInbox{tasks: []Task{task("t", "submitted", Message{Role: "user", Text: "x"})}}
	if err := Once(ctx, in, &fakeRunner{out: strings.Repeat("é", 100)}, Config{MaxReply: 101}); err != nil {
		t.Fatal(err)
	}
	if last := in.replies[len(in.replies)-1]; len(last.text) > 101 || !strings.HasSuffix(last.text, cutNote) {
		t.Fatalf("cut reply = %d bytes %q", len(last.text), last.text)
	}
}

func TestRunPauses(t *testing.T) {
	pause := filepath.Join(t.TempDir(), "pause")
	if err := os.WriteFile(pause, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	in := &fakeInbox{tasks: []Task{task("t", "submitted", Message{Role: "user", Text: "x"})}}
	r := &fakeRunner{out: "ok"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, in, r, Config{Interval: 10 * time.Millisecond, PauseFile: pause}) }()
	time.Sleep(60 * time.Millisecond)
	in.mu.Lock()
	n := len(in.replies)
	in.mu.Unlock()
	if n != 0 {
		t.Fatalf("paused loop replied %d times", n)
	}
	if err := os.Remove(pause); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		in.mu.Lock()
		n = len(in.replies)
		in.mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil || n != 2 {
		t.Fatalf("after the pause: %d replies, %v", n, err)
	}
}

func TestCommandStdinAndArg(t *testing.T) {
	out, err := Command{Name: "cat"}.Run(context.Background(), "hello")
	if err != nil || out != "hello" {
		t.Fatalf("stdin: %q, %v", out, err)
	}
	out, err = Command{Name: "echo", PromptArg: "-p="}.Run(context.Background(), "hi")
	if err != nil || strings.TrimSpace(out) != "-p=hi" {
		t.Fatalf("arg: %q, %v", out, err)
	}
	if got := (Command{Dir: "/home/me/repo"}).local("see [a.go](file:///home/me/repo/a.go#L3) and /home/me/repo/b.go"); got != "see [a.go](a.go#L3) and b.go" {
		t.Fatalf("local paths: %q", got)
	}
	if _, err := Preset("gpt", "."); err == nil {
		t.Fatal("unknown preset accepted")
	}
}

func TestRunWaitsOnTheServer(t *testing.T) {
	in := &fakeInbox{waits: true, tasks: []Task{task("t", "submitted", Message{Role: "user", Text: "x"})}}
	in.tasks[0].Updated = in.tick()
	r := &fakeRunner{out: "ok"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// A long Interval: a loop that waits on the server never sleeps it.
	go func() { done <- Run(ctx, in, r, Config{Interval: time.Hour, Wait: 400 * time.Millisecond}) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		in.mu.Lock()
		n := len(in.replies)
		in.mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A task that arrives later is answered too, without an Interval sleep.
	in.mu.Lock()
	late := task("late", "submitted", Message{Role: "user", Text: "y"})
	late.Updated = in.tick()
	in.tasks = append(in.tasks, late)
	in.mu.Unlock()
	for time.Now().Before(deadline.Add(time.Second)) {
		in.mu.Lock()
		n := len(in.replies)
		in.mu.Unlock()
		if n == 4 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil || len(in.replies) != 4 || len(r.prompts) != 2 {
		t.Fatalf("replies %+v, prompts %d, %v", in.replies, len(r.prompts), err)
	}
}

func TestRunPollsAServerThatCannotWait(t *testing.T) {
	in := &fakeInbox{tasks: []Task{task("asked", "input-required", Message{Role: "user", Text: "x"}, Message{Role: "agent", Text: "q?"})}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := Run(ctx, in, &fakeRunner{}, Config{Interval: 100 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	// Without the fallback this loop would call the inbox thousands of times.
	if in.calls > 5 {
		t.Fatalf("%d inbox calls in 300ms", in.calls)
	}
}
