// Package ssrf guards server-side fetches against SSRF. A fetcher runs on a host
// next to localhost sidecars (Ollama, Playwright, DICTATOR) and on a Tailscale
// network, so a user-supplied URL must not be allowed to reach internal targets.
// CheckURL rejects non-http(s) schemes and hosts that resolve to loopback,
// private, link-local, ULA, multicast, unspecified, or Tailscale CGNAT
// addresses; Control re-checks the actually-dialed IP to defeat DNS rebinding.
package ssrf

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

// HTTPClient returns an http.Client whose dialer refuses internal destination IPs
// (via Control, defeating DNS rebinding at dial time) and which caps redirects and
// the given total timeout. Use it for any fetch of an operator/user-supplied URL
// (e.g. a source feed). Callers that also know the URL up front should CheckURL it
// first.
func HTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: Control}
	return &http.Client{
		Transport: &http.Transport{DialContext: dialer.DialContext},
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("ssrf: too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("ssrf: redirect to non-http scheme %q", req.URL.Scheme)
			}
			return nil
		},
	}
}

// tailscaleCGNAT is the 100.64.0.0/10 range Tailscale assigns to peers.
var tailscaleCGNAT = netip.MustParsePrefix("100.64.0.0/10")

// IsBlockedIP reports whether ip is in a range a server-side fetch must not reach.
func IsBlockedIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true // unparseable -> block
	}
	a = a.Unmap()
	return a.IsLoopback() ||
		a.IsUnspecified() ||
		a.IsLinkLocalUnicast() || // 169.254/16 (incl. cloud metadata), fe80::/10
		a.IsMulticast() ||
		a.IsPrivate() || // RFC1918 + ULA fc00::/7
		tailscaleCGNAT.Contains(a)
}

// Resolver looks up host -> IPs (net.DefaultResolver in production; injectable
// for tests).
type Resolver interface {
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
}

// Guard validates fetch targets.
type Guard struct{ resolver Resolver }

// New returns a Guard using the system resolver.
func New() *Guard { return &Guard{resolver: net.DefaultResolver} }

// CheckURL rejects a URL whose scheme is not http/https or whose host resolves
// to any blocked IP. Call it before fetching; also wire Control into the dialer,
// since the name resolution here and the eventual connect can differ (rebinding).
func (g *Guard) CheckURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("ssrf: parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("ssrf: scheme %q not allowed (http/https only)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("ssrf: empty host")
	}
	if ip := net.ParseIP(host); ip != nil { // literal IP host
		if IsBlockedIP(ip) {
			return fmt.Errorf("ssrf: %s is a blocked address", host)
		}
		return nil
	}
	ips, err := g.resolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("ssrf: resolve %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("ssrf: %q resolved to no addresses", host)
	}
	for _, ip := range ips {
		if IsBlockedIP(ip) {
			return fmt.Errorf("ssrf: %s resolves to a blocked address %s", host, ip)
		}
	}
	return nil
}

// Control is a net.Dialer.Control hook that rejects a connection whose resolved
// destination IP is blocked, defeating DNS rebinding (the hostname was checked
// in CheckURL; this checks the IP actually being dialed). Wire it via
// net.Dialer{Control: ssrf.Control} on the fetcher's http.Transport.
func Control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("ssrf: bad dial address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || IsBlockedIP(ip) {
		return fmt.Errorf("ssrf: refusing to dial blocked address %s", host)
	}
	return nil
}
