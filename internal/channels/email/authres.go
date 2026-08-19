package email

import "strings"

// Sender authentication: the RFC From address is trivially spoofable, so before a sender is
// matched against the allow-list, the receiving provider's SPF/DKIM verdict must vouch for it.
// The mailbox provider records that verdict in the Authentication-Results header it PREPENDS on
// receipt (RFC 8601) — so the topmost instance is the trustworthy one; any instance below it
// could have been written by the sender themselves. The gate is FAIL CLOSED: no topmost
// Authentication-Results, or no aligned spf=pass / dkim=pass in it, means the message is
// refused (logged and skipped durably in pollOnce). A mailbox provider that adds no
// Authentication-Results header at all therefore cannot authorize anyone — that is the
// documented trade-off: an unauthenticatable identity must never drive the agent.

// senderAuthenticated reports whether the topmost Authentication-Results header carries an
// SPF or DKIM pass aligned (relaxed: equal, or subdomain on a dot boundary) with the RFC
// From domain.
func senderAuthenticated(p parsedMessage) bool {
	domain := domainOf(p.from)
	if domain == "" || len(p.authResults) == 0 {
		return false
	}
	ar := stripComments(strings.ToLower(p.authResults[0]))
	for _, clause := range strings.Split(ar, ";") {
		fields := strings.Fields(clause)
		if len(fields) == 0 {
			continue
		}
		method, verdict, ok := strings.Cut(fields[0], "=")
		if !ok || strings.TrimSpace(verdict) != "pass" {
			continue
		}
		props := fields[1:]
		switch strings.TrimSpace(method) {
		case "spf":
			if propAligned(props, "smtp.mailfrom", domain) || propAligned(props, "smtp.helo", domain) {
				return true
			}
		case "dkim":
			if propAligned(props, "header.d", domain) || propAligned(props, "header.i", domain) {
				return true
			}
		}
	}
	return false
}

// propAligned reports whether any key=value property names a domain aligned with the From
// domain. Values that are addresses (smtp.mailfrom, header.i) are reduced to their domain.
func propAligned(props []string, key, fromDomain string) bool {
	for _, kv := range props {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		if i := strings.LastIndexByte(v, '@'); i >= 0 {
			v = v[i+1:]
		}
		if domainAligned(v, fromDomain) {
			return true
		}
	}
	return false
}

// domainAligned implements relaxed alignment: the authenticated domain and the From domain are
// equal, or one is a subdomain of the other on a dot boundary.
func domainAligned(d, from string) bool {
	d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
	from = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(from)), ".")
	if d == "" || from == "" {
		return false
	}
	return d == from || strings.HasSuffix(from, "."+d) || strings.HasSuffix(d, "."+from)
}

// domainOf returns the domain of an address, "" when there is none.
func domainOf(addr string) string {
	i := strings.LastIndexByte(addr, '@')
	if i < 0 || i == len(addr)-1 {
		return ""
	}
	return addr[i+1:]
}

// stripComments removes RFC 5322 parenthesized comments (possibly nested) so free-text like
// "spf=pass (google.com: domain of a@b designates …)" can't confuse the property scan.
func stripComments(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '(':
			depth++
		case r == ')' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}
