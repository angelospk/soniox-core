package server

import (
	"net/http/httptest"
	"testing"
)

func TestOriginAllowed(t *testing.T) {
	extra := parseAllowedOrigins(" https://minipc.tail.ts.net:9443 ,https://Other.example ")
	cases := []struct {
		origin string
		want   bool
	}{
		{"", true}, // native client, no Origin
		{"http://localhost:5173", true},
		{"http://127.0.0.1:5173", true},
		{"https://minipc.tail.ts.net:9443", true},
		{"https://other.example", true},           // case-insensitive
		{"https://minipc.tail.ts.net", false},     // port must match exactly
		{"http://minipc.tail.ts.net:9443", false}, // scheme must match exactly
		{"https://evil.minipc.tail.ts.net:9443", false},
		{"https://evil.example", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/live", nil)
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if got := originAllowed(r, extra); got != c.want {
			t.Errorf("origin %q: got %v, want %v", c.origin, got, c.want)
		}
	}
}

func TestOriginAllowedNoExtra(t *testing.T) {
	r := httptest.NewRequest("GET", "/live", nil)
	r.Header.Set("Origin", "https://minipc.tail.ts.net:9443")
	if originAllowed(r, parseAllowedOrigins("")) {
		t.Fatal("non-local origin must be rejected without an allowlist")
	}
}
