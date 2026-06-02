// Package textutil holds small, dependency-free text helpers shared across the
// enrichment and retrieval layers.
package textutil

import "unicode/utf8"

// TruncateBytes caps s to at most n bytes without splitting a UTF-8 rune: it
// trims back to the last whole rune at or before n, so a caller (e.g. an LLM
// prompt builder) never emits a corrupted trailing character.
func TruncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.RuneStart(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	// The kept prefix may still end in a lone lead byte of a multi-byte rune; drop
	// it if it no longer decodes cleanly.
	if r, size := utf8.DecodeLastRuneInString(s); r == utf8.RuneError && size <= 1 {
		s = s[:len(s)-size]
	}
	return s
}
