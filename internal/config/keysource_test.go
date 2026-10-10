package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// isolate points the key cache at a fresh HOME and clears every key source, so
// each test states exactly the sources it relies on.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SONIOX_API_KEY", "")
	t.Setenv("SONIOX_TEMP_KEY", "")
	t.Setenv("SONIOX_KEY_COMMAND", "")
	return home
}

func writeCache(t *testing.T, key string, exp time.Time) {
	t.Helper()
	p := cachePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cachedKey{APIKey: key, ExpiresAt: exp.UTC().Format(time.RFC3339)})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readCache(t *testing.T) cachedKey {
	t.Helper()
	b, err := os.ReadFile(cachePath())
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var c cachedKey
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("decode cache: %v", err)
	}
	return c
}

// markerCommand returns a key command that records that it ran, then runs body.
func markerCommand(t *testing.T, body string) (cmd, marker string) {
	t.Helper()
	marker = filepath.Join(t.TempDir(), "ran")
	return "touch '" + marker + "'; " + body, marker
}

func ran(marker string) bool {
	_, err := os.Stat(marker)
	return err == nil
}

func future() string { return time.Now().Add(time.Hour).UTC().Format(time.RFC3339) }

func TestResolve_StaticKeyWins(t *testing.T) {
	isolate(t)
	t.Setenv("SONIOX_API_KEY", "static-key")
	cmd, marker := markerCommand(t, "echo other")
	t.Setenv("SONIOX_KEY_COMMAND", cmd)

	got, err := ResolveSonioxKey(context.Background())
	if err != nil || got != "static-key" {
		t.Fatalf("got %q, %v; want static-key", got, err)
	}
	if ran(marker) {
		t.Fatal("key command must not run when a static key is set")
	}
}

func TestResolve_FreshCacheSkipsCommand(t *testing.T) {
	isolate(t)
	writeCache(t, "cached-key", time.Now().Add(30*time.Minute))
	cmd, marker := markerCommand(t, "echo other")
	t.Setenv("SONIOX_KEY_COMMAND", cmd)

	got, err := ResolveSonioxKey(context.Background())
	if err != nil || got != "cached-key" {
		t.Fatalf("got %q, %v; want cached-key", got, err)
	}
	if ran(marker) {
		t.Fatal("key command must not run while the cached key is fresh")
	}
}

func TestResolve_CommandJSONIsCached(t *testing.T) {
	isolate(t)
	writeCache(t, "old-key", time.Now().Add(30*time.Second)) // inside the refresh margin
	exp := future()
	t.Setenv("SONIOX_KEY_COMMAND", `printf '{"api_key":"cmd-key","expires_at":"`+exp+`"}'`)

	got, err := ResolveSonioxKey(context.Background())
	if err != nil || got != "cmd-key" {
		t.Fatalf("got %q, %v; want cmd-key", got, err)
	}
	if c := readCache(t); c.APIKey != "cmd-key" || c.ExpiresAt != exp {
		t.Fatalf("cache = %+v, want cmd-key/%s", c, exp)
	}
}

func TestResolve_CommandRawKeyNotCached(t *testing.T) {
	isolate(t)
	t.Setenv("SONIOX_KEY_COMMAND", "echo '  raw-key  '")

	got, err := ResolveSonioxKey(context.Background())
	if err != nil || got != "raw-key" {
		t.Fatalf("got %q, %v; want raw-key", got, err)
	}
	if _, err := os.Stat(cachePath()); err == nil {
		t.Fatal("a key without an expiry must not be cached")
	}
}

func TestResolve_CommandExpiredKeyRejected(t *testing.T) {
	isolate(t)
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	t.Setenv("SONIOX_KEY_COMMAND", `printf '{"api_key":"dead","expires_at":"`+past+`"}'`)

	if got, err := ResolveSonioxKey(context.Background()); err == nil {
		t.Fatalf("got %q, want an error for an expired key", got)
	}
}

func TestResolve_CommandFailure(t *testing.T) {
	isolate(t)
	t.Setenv("SONIOX_KEY_COMMAND", "exit 3")

	if _, err := ResolveSonioxKey(context.Background()); err == nil {
		t.Fatal("want an error when the key command fails")
	}
}

func TestResolve_NoSourceNamesAPIKey(t *testing.T) {
	isolate(t)

	_, err := ResolveSonioxKey(context.Background())
	if err == nil || !strings.Contains(err.Error(), "SONIOX_API_KEY") {
		t.Fatalf("err = %v, want it to point at SONIOX_API_KEY", err)
	}
}

func TestForceRefresh_StaticKeyIsNoOp(t *testing.T) {
	isolate(t)
	t.Setenv("SONIOX_API_KEY", "static-key-123")

	st, err := ForceRefreshSonioxKey(context.Background())
	if err != nil || !st.OK || st.Source != "static" {
		t.Fatalf("got %+v, %v; want OK static", st, err)
	}
}

func TestForceRefresh_RunsCommandEvenWithFreshCache(t *testing.T) {
	isolate(t)
	writeCache(t, "old-key", time.Now().Add(30*time.Minute))
	exp := future()
	t.Setenv("SONIOX_KEY_COMMAND", `printf '{"api_key":"new-key","expires_at":"`+exp+`"}'`)

	st, err := ForceRefreshSonioxKey(context.Background())
	if err != nil || !st.OK || st.Source != "minted" || st.ExpiresAt != exp {
		t.Fatalf("got %+v, %v; want OK minted %s", st, err, exp)
	}
	if c := readCache(t); c.APIKey != "new-key" {
		t.Fatalf("cache key = %q, want new-key", c.APIKey)
	}
}

func TestForceRefresh_BareKeyDropsStaleCache(t *testing.T) {
	isolate(t)
	writeCache(t, "old-key", time.Now().Add(30*time.Minute))
	t.Setenv("SONIOX_KEY_COMMAND", "echo bare-key")

	st, err := ForceRefreshSonioxKey(context.Background())
	if err != nil || !st.OK {
		t.Fatalf("got %+v, %v; want OK", st, err)
	}
	// The old cached key must not shadow the fresh bare key on the next resolve.
	got, err := ResolveSonioxKey(context.Background())
	if err != nil || got != "bare-key" {
		t.Fatalf("resolve after refresh = %q, %v; want bare-key", got, err)
	}
}

func TestResolve_CommandTimeoutNotBlockedByChild(t *testing.T) {
	isolate(t)
	defer func(d, w time.Duration) { keyCommandTimeout, keyCommandWaitDelay = d, w }(keyCommandTimeout, keyCommandWaitDelay)
	keyCommandTimeout, keyCommandWaitDelay = 200*time.Millisecond, 200*time.Millisecond
	// A background child keeps stdout open long after the shell is killed.
	t.Setenv("SONIOX_KEY_COMMAND", "sleep 10 2>/dev/null & sleep 10 2>/dev/null")

	start := time.Now()
	if _, err := ResolveSonioxKey(context.Background()); err == nil {
		t.Fatal("want a timeout error")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("resolve took %v; the timeout must bound it", d)
	}
}

func TestForceRefresh_FailurePassesHintAndKeepsCache(t *testing.T) {
	isolate(t)
	writeCache(t, "still-good", time.Now().Add(30*time.Minute))
	t.Setenv("SONIOX_KEY_COMMAND", `printf '{"hint":"network"}'; exit 1`)

	st, err := ForceRefreshSonioxKey(context.Background())
	if err == nil || st.OK || st.Hint != "network" {
		t.Fatalf("got %+v, %v; want !OK with hint network", st, err)
	}
	if c := readCache(t); c.APIKey != "still-good" {
		t.Fatal("a failed refresh must leave the cached key intact")
	}
}

func TestForceRefresh_FailureWithoutHint(t *testing.T) {
	isolate(t)
	t.Setenv("SONIOX_KEY_COMMAND", "exit 1")

	st, err := ForceRefreshSonioxKey(context.Background())
	if err == nil || st.OK || st.Hint != hintUnavailable {
		t.Fatalf("got %+v, %v; want !OK with hint %q", st, err, hintUnavailable)
	}
}

func TestForceRefresh_NoSource(t *testing.T) {
	isolate(t)

	st, err := ForceRefreshSonioxKey(context.Background())
	if err == nil || st.OK {
		t.Fatalf("got %+v, %v; want a failure with no key source", st, err)
	}
}

func TestSaveTempKey_ConcurrentWritersNeverPublishPartialFile(t *testing.T) {
	isolate(t)
	exp := future()
	if err := saveTempKey("seed", exp); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8*50)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if err := saveTempKey(fmt.Sprintf("key-%d-%d-%s", w, i, strings.Repeat("x", 200)), exp); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	for {
		select {
		case <-done:
			close(errs)
			for err := range errs {
				t.Fatalf("concurrent write failed: %v", err)
			}
			if m, _ := filepath.Glob(filepath.Join(filepath.Dir(cachePath()), ".temp_key-*")); len(m) > 0 {
				t.Fatalf("leftover temp files: %v", m)
			}
			return
		default:
			if _, ok := cachedTempKey(); !ok {
				t.Fatal("a reader saw a missing or partial cache file")
			}
		}
	}
}

func TestForceRefresh_CacheWriteFailureIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	isolate(t)
	writeCache(t, "old-key", time.Now().Add(30*time.Minute))
	dir := filepath.Dir(cachePath())
	if err := os.Chmod(dir, 0o500); err != nil { // no new files, no removal
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	t.Setenv("SONIOX_KEY_COMMAND", `printf '{"api_key":"new-key","expires_at":"`+future()+`"}'`)

	// The new key can't replace the old cached one, so a success would be a lie.
	st, err := ForceRefreshSonioxKey(context.Background())
	if err == nil || st.OK {
		t.Fatalf("got %+v, %v; want a failure when the cache can't be updated", st, err)
	}
}

func TestResolve_BadExpiryDoesNotEchoOutput(t *testing.T) {
	isolate(t)
	t.Setenv("SONIOX_KEY_COMMAND", `printf '{"api_key":"k","expires_at":"secret-ish-value"}'`)

	_, err := ResolveSonioxKey(context.Background())
	if err == nil || strings.Contains(err.Error(), "secret-ish-value") {
		t.Fatalf("err = %v; want an error that does not echo the output", err)
	}
}
