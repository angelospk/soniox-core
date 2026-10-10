package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseLine(t *testing.T) {
	cases := []struct {
		in   string
		k, v string
		ok   bool
	}{
		{"GEMINI_API_KEY=abc123", "GEMINI_API_KEY", "abc123", true},
		{"  KEY = value with spaces ", "KEY", "value with spaces", true},
		{`Q="quoted"`, "Q", "quoted", true},
		{"S='single'", "S", "single", true},
		{"# comment", "", "", false},
		{"", "", "", false},
		{"noequals", "", "", false},
		{"=novalue", "", "", false},
		{"URL=https://x.y/z?a=b", "URL", "https://x.y/z?a=b", true}, // only first '=' splits
	}
	for _, c := range cases {
		k, v, ok := parseLine(c.in)
		if k != c.k || v != c.v || ok != c.ok {
			t.Errorf("parseLine(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, k, v, ok, c.k, c.v, c.ok)
		}
	}
}

func TestLoadDotEnvNoOverride(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	os.WriteFile(p, []byte("FOO=fromfile\nBAR=barval\n"), 0o600)

	os.Setenv("FOO", "preset")
	defer os.Unsetenv("FOO")
	defer os.Unsetenv("BAR")

	if err := LoadDotEnv(p); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("FOO"); got != "preset" {
		t.Errorf("FOO overridden: got %q, want preset", got)
	}
	if got := os.Getenv("BAR"); got != "barval" {
		t.Errorf("BAR not loaded: got %q", got)
	}
}

func TestLoadDotEnvMissingOK(t *testing.T) {
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "nope.env")); err != nil {
		t.Errorf("missing file should be nil, got %v", err)
	}
}

func TestSonioxKeyPrecedence(t *testing.T) {
	defer os.Unsetenv("SONIOX_API_KEY")
	defer os.Unsetenv("SONIOX_TEMP_KEY")

	os.Setenv("SONIOX_TEMP_KEY", "temp")
	os.Unsetenv("SONIOX_API_KEY")
	if got := SonioxKey(); got != "temp" {
		t.Errorf("want temp, got %q", got)
	}
	os.Setenv("SONIOX_API_KEY", "real")
	if got := SonioxKey(); got != "real" {
		t.Errorf("real should win, got %q", got)
	}
}
