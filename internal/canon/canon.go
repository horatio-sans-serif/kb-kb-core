// Package canon normalizes URLs and fingerprints content so the same artifact
// arriving at differently-decorated URLs, or as a re-formatted copy, collapses
// to one canonical artifact (design §8.2).
package canon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// trackingParams are query keys stripped during canonicalization (in addition to
// any key with the "utm_" prefix). "amp" is included to de-AMP query-style AMP.
var trackingParams = map[string]bool{
	"fbclid": true, "gclid": true, "dclid": true, "gclsrc": true, "msclkid": true,
	"mc_eid": true, "mc_cid": true, "igshid": true, "ref": true, "ref_src": true,
	"ref_url": true, "_hsenc": true, "_hsmi": true, "vero_id": true,
	"oly_enc_id": true, "oly_anon_id": true, "amp": true,
}

// URL returns a canonical form of raw: lowercased scheme/host, no userinfo, no
// default port, no leading "www.", tracking params stripped, remaining query
// sorted, fragment dropped, trailing slash removed (except root), conservative
// de-AMP. It is pure and offline; redirect-following is done at fetch time
// (Phase 2), which re-canonicalizes the resolved URL.
func URL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("not an absolute url: %q", raw)
	}

	u.Scheme = strings.ToLower(u.Scheme)
	u.User = nil

	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		u.Host = host + ":" + port
	} else {
		u.Host = host
	}

	path := stripAMPPath(u.Path)
	if path == "" {
		path = "/"
	} else if path != "/" {
		if path = strings.TrimRight(path, "/"); path == "" {
			path = "/"
		}
	}
	u.Path = path
	u.RawPath = ""

	q := u.Query()
	for k := range q {
		if strings.HasPrefix(strings.ToLower(k), "utm_") || trackingParams[strings.ToLower(k)] {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode() // Encode sorts by key

	u.Fragment = ""
	u.RawFragment = ""

	return u.String(), nil
}

// stripAMPPath removes a trailing "/amp" path segment (conservative de-AMP).
func stripAMPPath(p string) string {
	trimmed := strings.TrimRight(p, "/")
	if strings.HasSuffix(trimmed, "/amp") {
		return strings.TrimSuffix(trimmed, "/amp")
	}
	return p
}

// Content returns a stable, whitespace-insensitive fingerprint of body
// (SHA-256 over whitespace-collapsed text). This catches verbatim and
// reformatted copies; near-duplicate detection (SimHash/MinHash) is a later
// refinement (§8.2).
func Content(body string) string {
	norm := strings.Join(strings.Fields(body), " ")
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}
