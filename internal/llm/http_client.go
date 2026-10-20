package llm

import (
	"net/http"
	"time"
)

// vendorIdleConnsPerHost bounds the keep-alive pool per vendor host. Go's default of 2
// makes every concurrent request beyond two open a fresh connection; at a few thousand
// RPS the closed ones pile up in TIME_WAIT until outbound ports run out.
const vendorIdleConnsPerHost = 1024

var vendorHTTPClient = &http.Client{Transport: newVendorTransport()}

// VendorHTTPClient is the shared client for calls to model vendors and local nodes,
// so every adapter reuses one connection pool. It sets no timeout: callers bound each
// call with their context.
func VendorHTTPClient() *http.Client { return vendorHTTPClient }

func newVendorTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 0 // no global cap; the per-host cap below bounds the pool
	t.MaxIdleConnsPerHost = vendorIdleConnsPerHost
	t.IdleConnTimeout = 90 * time.Second
	return t
}
