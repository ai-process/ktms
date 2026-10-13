package nodes

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ValidateEndpoint checks that the endpoint URL is valid HTTP/HTTPS and prevents SSRF
// against localhost/loopback/link-local/cloud-metadata unless allowLocalhost is enabled.
func ValidateEndpoint(ctx context.Context, endpoint string, allowLocalhost bool) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("invalid endpoint URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported scheme %q (must be http or https)", u.Scheme)
	}
	hostname := u.Hostname()
	if hostname == "" {
		return fmt.Errorf("empty hostname in endpoint")
	}

	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", hostname)
	if err != nil {
		// If DNS resolution fails, check if hostname is direct IP literal
		ip := net.ParseIP(hostname)
		if ip == nil {
			return fmt.Errorf("cannot resolve endpoint host %q: %w", hostname, err)
		}
		ips = []net.IP{ip}
	}

	for _, ip := range ips {
		if !allowLocalhost {
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
				return fmt.Errorf("SSRF protection: endpoint IP %s is loopback/link-local/unspecified", ip.String())
			}
			// Block AWS/GCP/Azure link-local metadata (169.254.169.254)
			if ip.Equal(net.ParseIP("169.254.169.254")) {
				return fmt.Errorf("SSRF protection: endpoint IP %s is cloud metadata service", ip.String())
			}
		}
	}
	return nil
}

// ValidateCIDRs checks whether remoteAddr belongs to any of the allowed CIDRs.
// If allowedCIDRs is empty, any remote address is permitted.
func ValidateCIDRs(remoteAddr string, allowedCIDRs []string) error {
	if len(allowedCIDRs) == 0 {
		return nil
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("invalid remote IP %q", host)
	}

	for _, cidr := range allowedCIDRs {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			// If not in CIDR notation, parse as direct IP
			targetIP := net.ParseIP(cidr)
			if targetIP != nil && targetIP.Equal(ip) {
				return nil
			}
			continue
		}
		if ipNet.Contains(ip) {
			return nil
		}
	}
	return fmt.Errorf("remote IP %s not in allowed CIDRs", host)
}
