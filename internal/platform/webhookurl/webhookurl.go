// Package webhookurl validates that an outbound webhook destination is
// safe to send to — shared by every module that delivers a webhook to a
// tenant-supplied URL, so the SSRF check exists in exactly one place
// rather than being duplicated (and potentially drifting) per caller.
package webhookurl

import (
	"fmt"
	"net"
	"net/url"
)

// Validate rejects anything that isn't https, and anything whose hostname
// resolves to a loopback/private/link-local address — including cloud
// metadata endpoints (169.254.169.254 falls under IsLinkLocalUnicast).
// This is the direct fix for the v1 audit's SSRF-via-webhook-URL finding,
// which only checked URL syntax and accepted internal addresses outright.
//
// Callers that both register a URL and later deliver to it should call
// Validate again immediately before each delivery, not just at
// registration time — DNS can rebind between the two.
func Validate(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("must use https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("missing host")
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("could not resolve host: %w", err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("host resolved to no addresses")
	}
	for _, ip := range ips {
		if isDisallowedIP(ip) {
			return fmt.Errorf("resolves to a private/internal address (%s)", ip)
		}
	}
	return nil
}

func isDisallowedIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
}
