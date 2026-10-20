package llm

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// With Go's default of 2 idle connections per host, this load opens hundreds of
// connections; the vendor client must keep it near one per concurrent caller.
func TestVendorHTTPClientReusesConnections(t *testing.T) {
	var opened atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	const workers, perWorker = 32, 50
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				resp, err := VendorHTTPClient().Get(srv.URL)
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	// Dials can race an idle connection being returned, so allow a little slack;
	// without pooling this load opens ~15x the number of callers.
	if n := opened.Load(); n > 2*workers {
		t.Fatalf("opened %d connections for %d concurrent callers; keep-alive pool is not reused", n, workers)
	}
}
