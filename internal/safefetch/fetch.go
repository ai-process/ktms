// Package safefetch validates and downloads external media URLs with SSRF protection.
package safefetch

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	ErrProhibitedScheme = errors.New("only https and data schemes are allowed")
	ErrProhibitedIP     = errors.New("prohibited IP address")
	ErrUnsupportedMIME  = errors.New("unsupported media MIME type")
	ErrPayloadTooLarge  = errors.New("media payload exceeds size limit")
)

// IsProhibitedIP reports whether ip is a loopback, private, link-local, or cloud-metadata address.
func IsProhibitedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// 127.0.0.0/8 (loopback), 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 (private)
		if ip4[0] == 127 || ip4[0] == 10 || ip4[0] == 0 {
			return true
		}
		if ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31 {
			return true
		}
		if ip4[0] == 192 && ip4[1] == 168 {
			return true
		}
		// 169.254.0.0/16 (link-local, cloud metadata)
		if ip4[0] == 169 && ip4[1] == 254 {
			return true
		}
		// 100.64.0.0/10 (CGNAT / Shared Address Space)
		if ip4[0] == 100 && (ip4[1] >= 64 && ip4[1] <= 127) {
			return true
		}
	}
	return false
}

// ParseDataURI decodes a data URI into its MIME type and payload bytes.
func ParseDataURI(uri string) (string, []byte, error) {
	if !strings.HasPrefix(uri, "data:") {
		return "", nil, errors.New("not a data URI")
	}
	comma := strings.IndexByte(uri, ',')
	if comma < 0 {
		return "", nil, errors.New("malformed data URI")
	}
	meta := uri[len("data:"):comma]
	payload := uri[comma+1:]
	isBase64 := false
	if strings.HasSuffix(meta, ";base64") {
		isBase64 = true
		meta = strings.TrimSuffix(meta, ";base64")
	}
	mimeType := strings.TrimSpace(meta)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	if isBase64 {
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return "", nil, fmt.Errorf("invalid base64 in data URI: %w", err)
		}
		return mimeType, data, nil
	}
	unescaped, err := url.QueryUnescape(payload)
	if err != nil {
		return mimeType, []byte(payload), nil
	}
	return mimeType, []byte(unescaped), nil
}

// ValidateMediaMimeType verifies that the MIME type corresponds to supported image, audio, or document formats.
func ValidateMediaMimeType(mime string) error {
	m := strings.ToLower(strings.TrimSpace(mime))
	if idx := strings.IndexByte(m, ';'); idx >= 0 {
		m = strings.TrimSpace(m[:idx])
	}
	switch m {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return nil
	case "audio/wav", "audio/mp3", "audio/mpeg", "audio/ogg", "audio/webm", "audio/x-wav":
		return nil
	case "application/pdf":
		return nil
	}
	return fmt.Errorf("%w: %q", ErrUnsupportedMIME, mime)
}

// FetchURL downloads external media over HTTPS, enforcing SSRF IP blocking and payload limits.
func FetchURL(ctx context.Context, rawURL string, maxBytes int64) ([]byte, string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.Scheme != "https" {
		return nil, "", ErrProhibitedScheme
	}
	hostname := parsed.Hostname()
	if hostname == "" {
		return nil, "", errors.New("missing hostname in URL")
	}

	// Pre-resolve DNS to fail fast on prohibited IPs before connecting.
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", hostname)
	if err != nil {
		return nil, "", fmt.Errorf("DNS resolution failed for %s: %w", hostname, err)
	}
	for _, ip := range ips {
		if IsProhibitedIP(ip) {
			return nil, "", fmt.Errorf("%w: %s resolves to prohibited address %s", ErrProhibitedIP, hostname, ip)
		}
	}

	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
	}
	transport := &http.Transport{
		DialContext: func(dialCtx context.Context, network, addr string) (net.Conn, error) {
			h, _, splitErr := net.SplitHostPort(addr)
			if splitErr != nil {
				return nil, splitErr
			}
			connIPs, lerr := net.DefaultResolver.LookupIP(dialCtx, "ip", h)
			if lerr != nil {
				return nil, lerr
			}
			for _, ip := range connIPs {
				if IsProhibitedIP(ip) {
					return nil, fmt.Errorf("%w: %s connects to prohibited address %s", ErrProhibitedIP, h, ip)
				}
			}
			return dialer.DialContext(dialCtx, network, addr)
		},
		TLSHandshakeTimeout: 5 * time.Second,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   15 * time.Second,
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("upstream returned HTTP status %d", resp.StatusCode)
	}

	if maxBytes <= 0 {
		maxBytes = 16 << 20
	}
	lr := io.LimitReader(resp.Body, maxBytes+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, "", fmt.Errorf("reading response: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, "", ErrPayloadTooLarge
	}

	mimeType := resp.Header.Get("Content-Type")
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = http.DetectContentType(data)
	}
	if idx := strings.IndexByte(mimeType, ';'); idx >= 0 {
		mimeType = strings.TrimSpace(mimeType[:idx])
	}

	if err := ValidateMediaMimeType(mimeType); err != nil {
		return nil, "", err
	}

	return data, mimeType, nil
}

// CheckHost fails with ErrProhibitedIP when host is, or resolves to, an
// address IsProhibitedIP refuses.
func CheckHost(ctx context.Context, host string) error {
	ips, err := lookup(ctx, host)
	if err != nil {
		return err
	}
	for _, ip := range ips {
		if IsProhibitedIP(ip) {
			return fmt.Errorf("%w: %s resolves to %s", ErrProhibitedIP, host, ip)
		}
	}
	return nil
}

// GuardedDial dials like d but refuses a prohibited address. It connects to
// the address it checked, so a DNS answer cannot change in between.
func GuardedDial(d *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := lookup(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if IsProhibitedIP(ip) {
				return nil, fmt.Errorf("%w: %s resolves to %s", ErrProhibitedIP, host, ip)
			}
		}
		var lastErr error
		for _, ip := range ips {
			conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}

func lookup(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("DNS resolution failed for %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("DNS resolution failed for %s: no addresses", host)
	}
	return ips, nil
}
