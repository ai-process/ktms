package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type pingFunc func(context.Context) error

func (f pingFunc) Ping(ctx context.Context) error { return f(ctx) }

func readyz(t *testing.T, s *HTTPServer) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec.Code, rec.Body.String()
}

func TestReadyzWhileDatabaseIsDown(t *testing.T) {
	down := pingFunc(func(context.Context) error { return errors.New("connection refused") })

	strict := NewHTTPServer(down, ":0", nil, time.Second)
	if code, _ := readyz(t, strict); code != http.StatusServiceUnavailable {
		t.Fatalf("without stale auth: %d, want 503 (no call can be served)", code)
	}

	degraded := NewHTTPServer(down, ":0", nil, time.Second).WithDegradedReadiness(5 * time.Minute)
	code, body := readyz(t, degraded)
	if code != http.StatusOK || !strings.Contains(body, `"database":"unavailable"`) {
		t.Fatalf("with stale auth: %d %s, want 200 degraded: pulling every replica would turn degraded into down", code, body)
	}

	// Once the cache window has passed since the database was last seen,
	// every key has expired and the replica can serve nothing.
	degraded.lastPingOK.Store(time.Now().Add(-6 * time.Minute).UnixNano())
	if code, _ := readyz(t, degraded); code != http.StatusServiceUnavailable {
		t.Fatalf("past the window: %d, want 503", code)
	}
}

func TestReadyzPingIsBoundedBelowTheProbeTimeout(t *testing.T) {
	hung := pingFunc(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	start := time.Now()
	if code, _ := readyz(t, NewHTTPServer(hung, ":0", nil, time.Second)); code != http.StatusServiceUnavailable {
		t.Fatalf("hung database: %d, want 503", code)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("readyz took %v; the kubelet probe gives up at 5s", elapsed)
	}
}
