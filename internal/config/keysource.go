package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// keyCommandTimeout bounds one SONIOX_KEY_COMMAND run, so a hung command can't
// stall a /live handshake indefinitely. keyCommandWaitDelay bounds the wait for
// its output after the kill: a background child of the shell can keep stdout
// open. Variables so tests can shorten them.
var (
	keyCommandTimeout   = 60 * time.Second
	keyCommandWaitDelay = 2 * time.Second
)

// hintUnavailable is the /reconnect hint when the key command fails without
// naming a more specific one.
const hintUnavailable = "unavailable"

// errNoKeySource is returned when neither a static key, a cached key, nor a key
// command is configured.
var errNoKeySource = errors.New("no Soniox key: set SONIOX_API_KEY (or SONIOX_KEY_COMMAND)")

// ResolveSonioxKey returns a usable Soniox key with this precedence:
//  1. SONIOX_API_KEY / SONIOX_TEMP_KEY
//  2. a cached, still-valid temporary key
//  3. the output of SONIOX_KEY_COMMAND (cached when it carries an expiry)
func ResolveSonioxKey(ctx context.Context) (string, error) {
	if k := SonioxKey(); k != "" {
		return k, nil
	}
	if k, ok := cachedTempKey(); ok {
		return k, nil
	}
	out, err := runKeyCommand(ctx)
	if err != nil {
		return "", err
	}
	return out.APIKey, nil
}

// keyCommandOutput is what SONIOX_KEY_COMMAND prints on stdout: either a bare
// key, or JSON {"api_key", "expires_at"} on success / {"hint"} on failure.
type keyCommandOutput struct {
	APIKey    string `json:"api_key"`
	ExpiresAt string `json:"expires_at"`
	Hint      string `json:"hint"`
}

// keyCommandError carries the hint the command printed, if any, for /reconnect.
type keyCommandError struct {
	hint string
	err  error
}

func (e *keyCommandError) Error() string { return e.err.Error() }
func (e *keyCommandError) Unwrap() error { return e.err }

// runKeyCommand runs SONIOX_KEY_COMMAND through the shell and parses its stdout.
// A key with an RFC3339 expiry is validated and cached; a bare key is returned
// as-is. Stderr goes to the server log.
func runKeyCommand(ctx context.Context) (keyCommandOutput, error) {
	command := strings.TrimSpace(os.Getenv("SONIOX_KEY_COMMAND"))
	if command == "" {
		return keyCommandOutput{}, errNoKeySource
	}
	cctx, cancel := context.WithTimeout(ctx, keyCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "/bin/sh", "-c", command) // #nosec G204 G702 -- the operator configures this command
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	cmd.WaitDelay = keyCommandWaitDelay
	runErr := cmd.Run()

	text := strings.TrimSpace(stdout.String())
	var out keyCommandOutput
	if strings.HasPrefix(text, "{") {
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			return out, &keyCommandError{err: fmt.Errorf("SONIOX_KEY_COMMAND: bad JSON: %w", err)}
		}
	} else {
		out.APIKey = text
	}
	if runErr != nil {
		return out, &keyCommandError{hint: out.Hint, err: fmt.Errorf("SONIOX_KEY_COMMAND failed: %w", runErr)}
	}
	if out.APIKey == "" {
		return out, &keyCommandError{hint: out.Hint, err: errors.New("SONIOX_KEY_COMMAND printed no key")}
	}
	if out.ExpiresAt == "" {
		// A bare key is not cached, so an older cached key must not shadow it.
		if err := dropCache(); err != nil {
			return out, &keyCommandError{err: fmt.Errorf("drop key cache: %w", err)}
		}
		return out, nil
	}
	// Fixed messages: never echo the command's output into logs or clients.
	exp, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil {
		return out, &keyCommandError{err: errors.New("SONIOX_KEY_COMMAND: expires_at is not RFC3339")}
	}
	if time.Until(exp) <= 0 {
		return out, &keyCommandError{err: errors.New("SONIOX_KEY_COMMAND: key already expired")}
	}
	if err := saveTempKey(out.APIKey, out.ExpiresAt); err != nil {
		// The key is good; only the cache failed. Drop the old entry so it can't
		// shadow this key on the next resolve.
		if derr := dropCache(); derr != nil {
			return out, &keyCommandError{err: fmt.Errorf("write key cache: %w", err)}
		}
		log.Printf("key cache: %v (key used uncached)", err)
	}
	return out, nil
}

// ---- temp-key cache ----

func cachePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "soniox-dictate", "temp_key.json")
}

type cachedKey struct {
	APIKey    string `json:"api_key"`
	ExpiresAt string `json:"expires_at"`
}

func cachedTempKey() (string, bool) {
	b, err := os.ReadFile(cachePath())
	if err != nil {
		return "", false
	}
	var c cachedKey
	if json.Unmarshal(b, &c) != nil || c.APIKey == "" {
		return "", false
	}
	exp, err := time.Parse(time.RFC3339, c.ExpiresAt)
	if err != nil {
		return "", false
	}
	if time.Until(exp) < 90*time.Second { // refresh margin
		return "", false
	}
	return c.APIKey, true
}

func saveTempKey(key, expiresAt string) error {
	p := cachePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil { // key cache dir: user-only
		return err
	}
	b, err := json.Marshal(cachedKey{APIKey: key, ExpiresAt: expiresAt}) // #nosec G117 -- intentional cache of the user's own short-lived temp key
	if err != nil {
		return err
	}
	// Atomic write (unique 0600 temp file + rename) so a concurrent reader — e.g. a
	// mid-stream /live reconnect resolving the key while /reconnect rewrites it —
	// never sees a half-written file, and two concurrent writers never share one
	// temp file.
	f, err := os.CreateTemp(filepath.Dir(p), ".temp_key-*.json")
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(f.Name(), p)
	}
	if werr != nil {
		_ = os.Remove(f.Name())
	}
	return werr
}

// dropCache removes the cached key; a missing cache is not an error.
func dropCache() error {
	if err := os.Remove(cachePath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ReconnectStatus is the user-facing outcome of a forced Soniox key refresh.
type ReconnectStatus struct {
	OK        bool   `json:"ok"`
	Source    string `json:"source"`               // "static" | "minted"
	ExpiresAt string `json:"expires_at,omitempty"` // RFC3339, when the command gave one
	Hint      string `json:"hint,omitempty"`       // next step on failure
}

// ForceRefreshSonioxKey re-runs SONIOX_KEY_COMMAND even if the cached key is
// still fresh, so the next /live picks up a fresh key. It returns a user-facing
// status; the error (if any) carries detail for logs. A static SONIOX_API_KEY
// makes this a no-op success.
//
// It does NOT drop the cache up front: runKeyCommand overwrites it only on
// success, so a failed refresh leaves any still-valid cached key intact.
func ForceRefreshSonioxKey(ctx context.Context) (ReconnectStatus, error) {
	if SonioxKey() != "" {
		return ReconnectStatus{OK: true, Source: "static"}, nil
	}
	out, err := runKeyCommand(ctx)
	if err != nil {
		hint := ""
		var kerr *keyCommandError
		if errors.As(err, &kerr) {
			hint = kerr.hint
			if hint == "" {
				hint = hintUnavailable
			}
		}
		return ReconnectStatus{OK: false, Hint: hint}, err
	}
	return ReconnectStatus{OK: true, Source: "minted", ExpiresAt: out.ExpiresAt}, nil
}
