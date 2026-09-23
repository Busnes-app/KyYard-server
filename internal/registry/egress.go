package registry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
)

var ErrPrivateDestination = errors.New("registry resolves to a private or reserved address")

var (
	cgnat = netip.MustParsePrefix("100.64.0.0/10")
	nat64 = netip.MustParsePrefix("64:ff9b::/96")
)

// checkAddr admits public unicast addresses, and with allowPrivate also private ranges and
// carrier-grade NAT. Loopback, unspecified, multicast and link-local are never admitted.
func checkAddr(a netip.Addr, allowPrivate bool) error {
	a = a.Unmap()
	if nat64.Contains(a) { // judge the IPv4 address a NAT64 gateway would reach
		b := a.As16()
		a = netip.AddrFrom4([4]byte(b[12:]))
	}
	switch {
	case !a.IsValid(), a.IsLoopback(), a.IsUnspecified(), a.IsMulticast(), a.IsLinkLocalUnicast():
		return fmt.Errorf("%w: %s", ErrPrivateDestination, a)
	case !allowPrivate && (a.IsPrivate() || cgnat.Contains(a)):
		return fmt.Errorf("%w: %s", ErrPrivateDestination, a)
	}
	return nil
}

// lookup resolves host once and returns the first address the guard admits; the transport
// dials only that address.
func (c *Client) lookup(ctx context.Context, host string) (netip.Addr, error) {
	var addrs []netip.Addr
	var err error
	if c.opts.DialAddr != nil {
		var a netip.Addr
		a, err = c.opts.DialAddr(ctx, host)
		addrs = []netip.Addr{a}
	} else {
		addrs, err = net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%w: %s: %w", ErrUnavailable, host, err)
	}
	for _, a := range addrs {
		a = a.Unmap()
		if (c.allowLoopback && a.IsLoopback()) || checkAddr(a, c.opts.AllowPrivate) == nil {
			return a, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("%w: %s", ErrPrivateDestination, host)
}
