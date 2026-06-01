package canon

import "testing"

func TestURL(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"lowercase host", "https://Example.COM/x", "https://example.com/x"},
		{"strip utm params", "https://example.com/x?utm_source=a&utm_medium=b&id=5", "https://example.com/x?id=5"},
		{"strip fbclid", "https://example.com/x?fbclid=abc", "https://example.com/x"},
		{"strip www", "https://www.example.com/x", "https://example.com/x"},
		{"default https port", "https://example.com:443/x", "https://example.com/x"},
		{"default http port", "http://example.com:80/x", "http://example.com/x"},
		{"keep non-default port", "https://example.com:8443/x", "https://example.com:8443/x"},
		{"trailing slash", "https://example.com/x/", "https://example.com/x"},
		{"root slash kept", "https://example.com/", "https://example.com/"},
		{"empty path", "https://example.com", "https://example.com/"},
		{"strip fragment", "https://example.com/x#section", "https://example.com/x"},
		{"sort query", "https://example.com/x?b=2&a=1", "https://example.com/x?a=1&b=2"},
		{"de-amp path", "https://example.com/x/amp", "https://example.com/x"},
		{"de-amp query", "https://example.com/x?amp=1", "https://example.com/x"},
		{"de-amp root", "https://example.com/amp", "https://example.com/"},
		{"strip userinfo", "https://user:pass@example.com/x", "https://example.com/x"},
		{"all at once", "HTTPS://WWW.Example.com:443/p/?fbclid=z&b=2&utm_source=x&a=1#frag", "https://example.com/p?a=1&b=2"},
	}
	for _, c := range cases {
		got, err := URL(c.in)
		if err != nil {
			t.Errorf("%s: URL(%q) error: %v", c.name, c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: URL(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
		// idempotence: canonicalizing a canonical URL is a no-op.
		if again, err := URL(got); err != nil || again != got {
			t.Errorf("%s: not idempotent: URL(%q) = %q (err %v)", c.name, got, again, err)
		}
	}
}

func TestURL_Invalid(t *testing.T) {
	for _, in := range []string{"", "not a url", "/relative/path", "mailto:x@y.com"} {
		if _, err := URL(in); err == nil {
			t.Errorf("URL(%q): expected an error", in)
		}
	}
}

func FuzzURL(f *testing.F) {
	for _, s := range []string{
		"https://example.com/x", "http://a.b/c?d=1&utm_source=x", "https://x",
		"HTTPS://WWW.A.com:443/p/amp?b=2&a=1#f", "ftp://x/y", "not a url",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		out, err := URL(raw)
		if err != nil {
			return // rejected inputs are fine; we only assert no panic + idempotence
		}
		out2, err := URL(out)
		if err != nil {
			t.Fatalf("URL(%q)=%q, then re-canonicalizing errored: %v", raw, out, err)
		}
		if out2 != out {
			t.Fatalf("not idempotent: URL(%q)=%q but URL(%q)=%q", raw, out, out, out2)
		}
	})
}

func TestContent_StableAndWhitespaceInsensitive(t *testing.T) {
	a := Content("hello   world\n")
	b := Content("hello world")
	if a != b {
		t.Errorf("whitespace differences should not change the hash: %q vs %q", a, b)
	}
	if len(a) != 64 {
		t.Errorf("expected a 64-char hex sha256, got %d chars", len(a))
	}
	if Content("hello world") == Content("hello there") {
		t.Error("different content produced the same hash")
	}
}
