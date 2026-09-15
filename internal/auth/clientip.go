package auth

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ClientIP returns the address a request should be attributed to: the rate limiter's key and
// the IP bound into a session. Both must agree, or a caller could be throttled as one client
// and recorded as another.
//
// trusted is the parsed KY_TRUSTED_PROXIES allowlist. It is empty by default, and then the
// peer address is the answer and every header is ignored: X-Forwarded-For is caller-supplied,
// so honouring it unconditionally lets anyone mint a fresh limiter bucket per request.
//
// When the peer is a trusted proxy, X-Forwarded-For is walked from the right, skipping
// entries that are themselves trusted proxies, and the first untrusted entry wins. Everything
// to its left was appended by a hop we do not trust and may be forged. An entry that does not
// parse as an IP ends the walk and the peer is used, since the rest of the chain sits behind
// something that is not speaking the protocol.
//
// X-Real-IP is ignored outright. It carries no chain, so it cannot be walked; a deployment
// that needs a client IP must forward X-Forwarded-For.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	peer := peerIP(r)
	peerAddr, err := netip.ParseAddr(peer)
	if err != nil || !isTrusted(peerAddr, trusted) {
		return peer
	}

	// Every header line, not just the first: a proxy that emits its own line instead of
	// appending (HAProxy's "option forwardfor") leaves the real chain in the last line, and
	// reading only the first would hand the walk straight back to the caller.
	parts := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		addr, err := parseForwardedEntry(strings.TrimSpace(parts[i]))
		if err != nil {
			return peer
		}
		if !isTrusted(addr, trusted) {
			return addr.String()
		}
	}
	return peer
}

// parseForwardedEntry reads one X-Forwarded-For entry as a client address. Most proxies
// append a bare IP, but Azure App Service and Application Gateway append "ip:port" (or
// "[ipv6]:port"); without stripping the port, every client behind them collapses onto one
// unparsable entry and the walk falls back to the proxy's own address.
func parseForwardedEntry(entry string) (netip.Addr, error) {
	if addr, err := netip.ParseAddr(entry); err == nil {
		return addr.Unmap(), nil
	}
	host, _, err := net.SplitHostPort(entry)
	if err != nil {
		return netip.Addr{}, err
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, err
	}
	return addr.Unmap(), nil
}

// peerIP is the transport peer: the host half of RemoteAddr, normalised so an IPv4-mapped
// IPv6 peer keys the same bucket as the plain IPv4 one.
func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = strings.Trim(r.RemoteAddr, "[]")
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Unmap().String()
	}
	return host
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
