package ssrf

import (
	"context"
	"net"
	"testing"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", "0.0.0.0", // loopback / unspecified
		"10.0.0.1", "172.16.5.4", "192.168.1.1", "fc00::1", // private + ULA
		"169.254.169.254", "fe80::1", // link-local (incl. cloud metadata)
		"100.64.0.1", // Tailscale CGNAT
		"224.0.0.1",  // multicast
	}
	for _, s := range blocked {
		if !IsBlockedIP(net.ParseIP(s)) {
			t.Errorf("IsBlockedIP(%s) = false, want true", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"} {
		if IsBlockedIP(net.ParseIP(s)) {
			t.Errorf("IsBlockedIP(%s) = true, want false", s)
		}
	}
}

type fakeResolver struct {
	ips []net.IP
	err error
}

func (f fakeResolver) LookupIP(context.Context, string, string) ([]net.IP, error) {
	return f.ips, f.err
}

func TestGuard_CheckURL(t *testing.T) {
	ctx := context.Background()
	for _, raw := range []string{"file:///etc/passwd", "ftp://example.com/x", "gopher://x/", "http://127.0.0.1:11434/api"} {
		if err := New().CheckURL(ctx, raw); err == nil {
			t.Errorf("CheckURL(%q) = nil, want rejection", raw)
		}
	}

	blocked := &Guard{resolver: fakeResolver{ips: []net.IP{net.ParseIP("10.0.0.5")}}}
	if err := blocked.CheckURL(ctx, "http://internal.example.com/x"); err == nil {
		t.Error("a host resolving to a private IP should be rejected")
	}

	ok := &Guard{resolver: fakeResolver{ips: []net.IP{net.ParseIP("93.184.216.34")}}}
	if err := ok.CheckURL(ctx, "http://example.com/x"); err != nil {
		t.Errorf("a public host should pass: %v", err)
	}

	// One blocked IP among several still fails (rebinding-style).
	mixed := &Guard{resolver: fakeResolver{ips: []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("127.0.0.1")}}}
	if err := mixed.CheckURL(ctx, "http://example.com/x"); err == nil {
		t.Error("any blocked resolved IP should fail the check")
	}
}

func TestControl_RejectsBlocked(t *testing.T) {
	if err := Control("tcp", "127.0.0.1:80", nil); err == nil {
		t.Error("Control should reject a loopback dial")
	}
	if err := Control("tcp", "100.64.1.2:443", nil); err == nil {
		t.Error("Control should reject a Tailscale CGNAT dial")
	}
	if err := Control("tcp", "8.8.8.8:443", nil); err != nil {
		t.Errorf("Control should allow a public dial: %v", err)
	}
}
