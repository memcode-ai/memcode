package channels

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// The media-download SSRF guard. Every channel that fetches an attachment from
// a URL taken (even indirectly) from an inbound message routes the fetch
// through SafeHTTPClient, so a hostile contentUrl / downloadUri / MediaUrl can
// never make the gateway reach a loopback, private, link-local, or cloud
// metadata address — the class OpenClaw shipped `dangerouslyAllowPrivateNetwork`
// to defend and Hermes' central SSRF floor blocks. The check runs at DIAL time
// on the resolved IP, so DNS names that resolve to an internal address (and
// DNS-rebinding across redirects) are caught too, not just literal IPs.

// blockedIP reports whether dialing ip would reach a non-public destination.
func blockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true // link-local covers 169.254.0.0/16 and fe80::/10 (cloud metadata lives at 169.254.169.254)
	}
	// Carrier-grade NAT (100.64.0.0/10) — often fronts internal infra.
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1]&0xc0 == 64 {
		return true
	}
	return false
}

// safeControl is a net.Dialer.Control hook: it runs after resolution with the
// actual ip:port about to be dialed, and refuses any non-public address.
func safeControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if blockedIP(net.ParseIP(host)) {
		return fmt.Errorf("refusing to connect to non-public address %s", host)
	}
	return nil
}

// SafeHTTPClient returns an HTTP client whose every connection (including those
// made following redirects) is refused if it would reach a non-public IP. Use
// it for ALL outbound fetches of URLs derived from inbound messages.
func SafeHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: safeControl}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
			DisableKeepAlives:     true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("stopped after %d redirects", len(via))
			}
			return nil // each redirect re-dials through safeControl, so an internal target is still blocked
		},
	}
}

// HostAllowed reports whether host (or a subdomain of it) is in suffixes — for
// gating a privileged credential to first-party hosts only. Suffixes are
// matched on a dot boundary, so "evil-botframework.com" does not match
// ".botframework.com".
func HostAllowed(host string, suffixes ...string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	for _, suf := range suffixes {
		suf = strings.ToLower(suf)
		if host == strings.TrimPrefix(suf, ".") || strings.HasSuffix(host, suf) {
			return true
		}
	}
	return false
}
