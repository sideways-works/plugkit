package repository

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// logoTimeout bounds a logo's fetch, from its dial to its last byte.
const logoTimeout = 10 * time.Second

var (
	errRedirect = errors.New("redirects, and a logo's fetch follows none")
	errAddress  = errors.New("is an address a logo is never fetched from")
)

// Prefixes netip has no predicate for: CGNAT, "this network", benchmarking, the reserved block
// with the broadcast address, and IPv4 within IPv6 (compatible, NAT64 and 6to4), whose IPv4
// address is checked as itself.
var (
	refusedPrefixes = []netip.Prefix{
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("240.0.0.0/4"),
	}
	compat = netip.MustParsePrefix("::/96")
	nat64  = netip.MustParsePrefix("64:ff9b::/96")
	sixTo4 = netip.MustParsePrefix("2002::/16")
)

// publicAddr tells whether a logo may be fetched from a: none of loopback (unless loopback is
// allowed, for tests), private, link-local, CGNAT, unspecified, multicast or reserved addresses.
func publicAddr(a netip.Addr, loopback bool) bool {
	a = a.Unmap()

	switch {
	case !a.IsValid():
		return false
	case a.IsLoopback():
		return loopback
	case a.Is6() && (compat.Contains(a) || nat64.Contains(a)):
		b := a.As16()

		return publicAddr(netip.AddrFrom4([4]byte(b[12:])), loopback)
	case a.Is6() && sixTo4.Contains(a):
		b := a.As16()

		return publicAddr(netip.AddrFrom4([4]byte(b[2:6])), loopback)
	case a.IsPrivate(), a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast(), a.IsInterfaceLocalMulticast(), a.IsMulticast(), a.IsUnspecified():
		return false
	}

	for _, p := range refusedPrefixes {
		if p.Contains(a) {
			return false
		}
	}

	return true
}

// dialCheck is the dialer's check of each address it connects to, after the name is resolved, so a
// name that resolves to another address the second time gets no further.
func dialCheck(loopback bool) func(network, address string, _ syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil || !publicAddr(ap.Addr(), loopback) {
			return fmt.Errorf("%s %w", address, errAddress)
		}

		return nil
	}
}

// logoClient fetches a logo over https alone: no proxy, cookie, credential or redirect, every
// address checked as it is dialed, and the whole fetch bounded by logoTimeout.
func logoClient(opt Options) *http.Client {
	dialer := &net.Dialer{Timeout: logoTimeout, Control: dialCheck(opt.AllowLocal)}

	return &http.Client{
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            dialer.DialContext,
			TLSClientConfig:        &tls.Config{RootCAs: opt.roots, MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:    logoTimeout,
			ResponseHeaderTimeout:  logoTimeout,
			MaxResponseHeaderBytes: 64 << 10,
			DisableKeepAlives:      true,
		},
		Timeout:       logoTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect },
	}
}

func download(ctx context.Context, u string, opt Options, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, logoTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err //nolint:wrapcheck // the caller says whose
	}

	resp, err := logoClient(opt).Do(req)
	if err != nil {
		return nil, err //nolint:wrapcheck // the caller says whose
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: %s", errStatus, resp.Status)
	case resp.ContentLength > limit:
		return nil, fmt.Errorf("%w: over %d bytes", errTooBig, limit)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))

	switch {
	case err != nil:
		return nil, err //nolint:wrapcheck // the caller says whose
	case int64(len(data)) > limit:
		return nil, fmt.Errorf("%w: over %d bytes", errTooBig, limit)
	}

	return data, nil
}

var (
	errStatus = errors.New("the server answered")
	errTooBig = errors.New("too big")
)

// keepFile writes data to p whole, by a rename.
func keepFile(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err //nolint:wrapcheck // dropped by the caller
	}

	tmp, err := os.CreateTemp(filepath.Dir(p), ".logo-")
	if err != nil {
		return err //nolint:wrapcheck // as above
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone once renamed

	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}

	if err == nil {
		err = os.Rename(tmp.Name(), p)
	}

	return err //nolint:wrapcheck // as above
}
