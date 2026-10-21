// Package redisguard fails Redis commands fast while Redis is unreachable.
// Per-command timeouts alone multiply: one request runs a dozen commands, so a
// blackholed Redis would still cost it seconds before every limit fails open.
package redisguard

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// ErrOpen is returned without network IO while the guard is open.
var ErrOpen = errors.New("redis unavailable (guard open)")

const (
	tripAfter = 3               // consecutive transport failures that open the guard
	openFor   = 2 * time.Second // then commands go through again; the next result decides
)

// Guard is a go-redis hook shared by every user of the client: throttles,
// caches and the node pool all see the same open/closed state.
type Guard struct {
	mu       sync.Mutex
	fails    int
	openedAt time.Time
	now      func() time.Time
}

func New() *Guard { return &Guard{now: time.Now} }

// DialHook passes dials through: they happen inside admitted commands, whose
// outcome is already reported, and a probe must be able to reconnect.
func (g *Guard) DialHook(next redis.DialHook) redis.DialHook { return next }

func (g *Guard) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := g.admit(); err != nil {
			cmd.SetErr(err)
			return err
		}
		err := next(ctx, cmd)
		g.report(ctx, err)
		return err
	}
}

func (g *Guard) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if err := g.admit(); err != nil {
			for _, c := range cmds {
				c.SetErr(err)
			}
			return err
		}
		err := next(ctx, cmds)
		g.report(ctx, err)
		return err
	}
}

// Open reports whether commands are currently refused.
func (g *Guard) Open() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fails >= tripAfter && g.now().Sub(g.openedAt) < openFor
}

// admit lets everything through once openFor has passed rather than a single
// probe: go-redis sends its own handshake commands through these hooks when it
// reconnects, and gating them would keep the guard from ever closing.
func (g *Guard) admit() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fails >= tripAfter && g.now().Sub(g.openedAt) < openFor {
		return ErrOpen
	}
	return nil
}

// report updates the state from one outcome. A call that ended with its
// caller's context done says nothing about Redis (ContextTimeoutEnabled turns
// a short caller deadline into a socket timeout), so it neither counts nor resets.
func (g *Guard) report(ctx context.Context, err error) {
	// Judged by the caller's ctx only: go-redis's own dial timeout also wraps
	// context.DeadlineExceeded, and that one is an outage.
	if err != nil && callerDone(ctx) {
		return
	}
	transport := isTransport(err)
	g.mu.Lock()
	defer g.mu.Unlock()
	wasOpen := g.fails >= tripAfter
	if !transport {
		if wasOpen {
			log.Info().Msg("redis reachable again, guard closed")
		}
		g.fails = 0
		return
	}
	g.fails++
	if g.fails >= tripAfter {
		if !wasOpen {
			log.Warn().Err(err).Dur("open_for", openFor).Msg("redis unreachable, failing redis calls fast")
		}
		g.openedAt = g.now()
	}
}

// callerDone also catches a deadline that has passed before its timer fired:
// the socket deadline equals it, so the read timeout can win that race.
func callerDone(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	d, ok := ctx.Deadline()
	return ok && !time.Now().Before(d)
}

// isTransport separates "Redis is not there" from answers Redis gave: a nil
// reply, a script error or OOM mean it is up and must not open the guard.
func isTransport(err error) bool {
	if err == nil || errors.Is(err, redis.Nil) || errors.Is(err, ErrOpen) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// A pooled connection to a Redis that went away reads EOF, not a net.Error.
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, redis.ErrPoolTimeout)
}

// Log picks the level for a failed Redis call: once the guard is open the
// outage was logged when it opened, so each fast-failed call is only Debug.
func Log(err error) *zerolog.Event {
	if errors.Is(err, ErrOpen) {
		return log.Debug()
	}
	return log.Warn()
}
