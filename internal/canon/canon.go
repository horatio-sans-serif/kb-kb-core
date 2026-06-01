// Package canon normalizes URLs and fingerprints content so the same artifact
// arriving at differently-decorated URLs, or as a re-formatted copy, collapses
// to one canonical artifact (design §8.2).
package canon

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// trackingParams is the set of query keys stripped during canonicalization (in
// addition to any key with the "utm_" prefix). "amp" de-AMPs query-style AMP.
var trackingParams = map[string]struct{}{
	"fbclid": {}, "gclid": {}, "dclid": {}, "gclsrc": {}, "msclkid": {},
	"mc_eid": {}, "mc_cid": {}, "igshid": {}, "ref": {}, "ref_src": {},
	"ref_url": {}, "_hsenc": {}, "_hsmi": {}, "vero_id": {},
	"oly_enc_id": {}, "oly_anon_id": {}, "amp": {},
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
		lk := strings.ToLower(k)
		if _, tracked := trackingParams[lk]; tracked || strings.HasPrefix(lk, "utm_") {
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
// (SHA-256 over whitespace-collapsed text). It streams words straight into the
// hasher, never materializing the normalized copy, so an 8 MiB body costs O(1)
// extra memory. This catches verbatim and reformatted copies; near-duplicate
// detection (SimHash/MinHash) is a later refinement (§8.2).
func Content(body string) string {
	h := sha256.New()
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20) // allow a single long token up to 16 MiB
	sc.Split(bufio.ScanWords)
	for first := true; sc.Scan(); first = false {
		if !first {
			h.Write([]byte{' '})
		}
		h.Write(sc.Bytes())
	}
	return hex.EncodeToString(h.Sum(nil))
}
