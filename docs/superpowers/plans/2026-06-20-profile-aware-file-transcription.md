# Profile-Aware File Transcription Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `POST /transcribe` use the same active profile, language, vocabulary, AI toggle, model chain, and cleanup metadata as live dictation.

**Architecture:** Keep `internal/transcribe.File` focused on producing raw text and media statistics. Extract shared preference-aware cleanup into a server method used by both `/live` and `/transcribe`, with an injectable cleanup function and transcription function for deterministic handler tests.

**Tech Stack:** Go, `net/http`, multipart forms, existing `internal/cleanup`, existing `internal/transcribe`, standard `testing`.

---

### Task 1: Add Injectable File Transcription

**Files:**
- Modify: `internal/server/server.go`
- Create: `internal/server/transcribe_test.go`

- [ ] **Step 1: Write the failing handler test**

Add a test-only fake through a new `Server.TranscribeFile` field:

```go
func TestTranscribeKeepsExistingFields(t *testing.T) {
	s := &Server{
		KeyResolver: func(context.Context) (string, error) { return "key", nil },
		TranscribeFile: func(context.Context, string, transcribe.FileOptions) (transcribe.Result, error) {
			return transcribe.Result{Raw: "raw text", Cleaned: "raw text", Jobs: 2, Segments: 3, Duration: 42}, nil
		},
	}
	req := multipartRequest(t, "meeting.m4a", []byte("audio"))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["raw"] != "raw text" || got["cleaned"] != "raw text" {
		t.Fatalf("missing legacy text fields: %#v", got)
	}
	if got["jobs"] != float64(2) || got["segments"] != float64(3) || got["duration_s"] != float64(42) {
		t.Fatalf("missing legacy stats: %#v", got)
	}
}
```

The helper creates `POST /transcribe` with a multipart field named `file`.

- [ ] **Step 2: Run the test and verify RED**

Run: `rtk go test ./internal/server -run TestTranscribeKeepsExistingFields -v`

Expected: compile failure because `Server.TranscribeFile` does not exist.

- [ ] **Step 3: Add the minimal injection seam**

Add:

```go
type transcribeFileFunc func(context.Context, string, transcribe.FileOptions) (transcribe.Result, error)

type Server struct {
	// existing fields...
	TranscribeFile transcribeFileFunc
}

func (s *Server) transcribeFile(ctx context.Context, path string, opt transcribe.FileOptions) (transcribe.Result, error) {
	if s.TranscribeFile != nil {
		return s.TranscribeFile(ctx, path, opt)
	}
	return transcribe.File(ctx, path, opt)
}
```

Change the handler to call `s.transcribeFile`.

- [ ] **Step 4: Run the focused test and full package tests**

Run: `rtk go test ./internal/server -run TestTranscribeKeepsExistingFields -v`

Expected: PASS.

Run: `rtk go test ./internal/server`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
rtk git add internal/server/server.go internal/server/transcribe_test.go
rtk git commit -m "test(server): inject file transcription"
```

### Task 2: Extract Shared Preference-Aware Cleanup

**Files:**
- Modify: `internal/server/server.go`
- Modify: `internal/server/transcribe_test.go`

- [ ] **Step 1: Write failing cleanup-policy tests**

Add an injectable cleanup seam:

```go
type cleanupFunc func(context.Context, string, string, cleanup.Options) cleanup.Result
```

Then test:

```go
func TestCleanFileUsesActiveProfileLanguageVocabularyAndModels(t *testing.T) {
	prefs := t.TempDir()
	writePref(t, prefs, "active-profile", "notes")
	writePref(t, prefs, "output-language", "el")
	writePref(t, prefs, "vocabulary.txt", "OpenCouncil")
	writePref(t, prefs, "cleanup-models", "model-a\nmodel-b")
	if err := os.MkdirAll(filepath.Join(prefs, "prompts"), 0o755); err != nil { t.Fatal(err) }
	writePref(t, filepath.Join(prefs, "prompts"), "notes.md", "# summarize: true\nCUSTOM NOTES")

	var got cleanup.Options
	s := &Server{
		Cleanup: true, GeminiKey: "gemini", PrefsDir: prefs,
		Clean: func(_ context.Context, _ string, text string, opt cleanup.Options) cleanup.Result {
			got = opt
			return cleanup.Result{Text: "cleaned", Outcome: cleanup.Cleaned, Model: "model-a"}
		},
	}
	text, meta := s.cleanText(context.Background(), "raw text", false)

	if text != "cleaned" || meta.Profile != "notes" || meta.Lang != "el" {
		t.Fatalf("text=%q meta=%+v", text, meta)
	}
	if got.Body != "CUSTOM NOTES" || got.Vocab != "OpenCouncil" || !got.AllowShrink {
		t.Fatalf("options=%+v", got)
	}
	if !slices.Equal([]string{"model-a", "model-b"}, got.Models) {
		t.Fatalf("models=%v", got.Models)
	}
}

func TestCleanFileHonorsAIDisabled(t *testing.T) {
	prefs := t.TempDir()
	writePref(t, prefs, "no-ai-cleanup", "")
	called := false
	s := &Server{
		Cleanup: true, GeminiKey: "gemini", PrefsDir: prefs,
		Clean: func(context.Context, string, string, cleanup.Options) cleanup.Result {
			called = true
			return cleanup.Result{}
		},
	}
	text, meta := s.cleanText(context.Background(), "raw text", false)
	if called || text != "raw text" || meta.Outcome != cleanup.FallbackDisabled {
		t.Fatalf("called=%v text=%q meta=%+v", called, text, meta)
	}
}
```

Add the standard-library `slices` import to the test file.

- [ ] **Step 2: Run tests and verify RED**

Run: `rtk go test ./internal/server -run 'TestCleanFile' -v`

Expected: compile failure because `Server.Clean` and `cleanText` do not exist.

- [ ] **Step 3: Implement the shared helper**

Add `Server.Clean cleanupFunc`, `s.cleanResult(...)`, and:

```go
func (s *Server) cleanText(ctx context.Context, full string, allowVoiceCommand bool) (string, cleanupMeta) {
	if strings.TrimSpace(full) == "" {
		return full, cleanupMeta{Outcome: cleanup.RawTooShort}
	}
	// Preserve the current PrefsDir=="" legacy behavior.
	// With prefs, load profiles, active profile, language, vocabulary, models,
	// and no-ai-cleanup on every request.
	// Only call cleanup.SelectProfile when allowVoiceCommand is true.
}
```

Refactor `cleanLive` to:

1. emit the `cleaning` event only when cleanup will run;
2. call `cleanText(ctx, full, true)`;
3. preserve the existing `cleanup_failed` advisory behavior.

The file path will later call `cleanText(ctx, raw, false)`.

- [ ] **Step 4: Run focused and full tests**

Run: `rtk go test ./internal/server -run 'TestCleanFile' -v`

Expected: PASS.

Run: `rtk go test ./internal/server`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
rtk git add internal/server/server.go internal/server/transcribe_test.go
rtk git commit -m "refactor(server): share cleanup preferences"
```

### Task 3: Return File Cleanup Metadata

**Files:**
- Modify: `internal/server/server.go`
- Modify: `internal/server/transcribe_test.go`
- Modify: `internal/transcribe/transcribe.go`
- Modify: `cmd/soniox/main.go`

- [ ] **Step 1: Write the failing endpoint metadata test**

Configure the fake transcriber to return raw text, configure `Server.Clean` to
return a deterministic cleaned result, then assert:

```go
if got["cleaned"] != "polished text" || got["profile"] != "default" || got["lang"] != "el" || got["model"] != "model-a" {
	t.Fatalf("response=%#v", got)
}
meta := got["cleanup"].(map[string]any)
if meta["outcome"] != "cleaned" || meta["reason"] != "" {
	t.Fatalf("cleanup=%#v", meta)
}
```

Also add a disabled-cleanup test that expects `cleaned == raw` and
`cleanup.outcome == "fallback_disabled"`.

- [ ] **Step 2: Run tests and verify RED**

Run: `rtk go test ./internal/server -run 'TestTranscribe.*Cleanup' -v`

Expected: assertions fail because `/transcribe` still returns no metadata and
still delegates cleanup to `transcribe.File`.

- [ ] **Step 3: Make transcription raw-only and clean at the server**

Remove `Cleanup`, `GeminiKey`, and `Vocab` from `transcribe.FileOptions`. Remove
the cleanup call from `transcribe.File`; set `Cleaned` to raw for CLI
backward compatibility.

In `handleTranscribe`:

```go
res, err := s.transcribeFile(r.Context(), tmp.Name(), transcribe.FileOptions{
	Key: key,
	Langs: s.Langs,
})
// error handling unchanged
cleaned, meta := s.cleanText(r.Context(), res.Raw, false)
res.Cleaned = cleaned
json.NewEncoder(w).Encode(map[string]any{
	"raw": res.Raw, "cleaned": res.Cleaned,
	"jobs": res.Jobs, "segments": res.Segments, "duration_s": res.Duration,
	"profile": meta.Profile, "lang": meta.Lang, "model": meta.Model,
	"cleanup": map[string]any{
		"outcome": string(meta.Outcome),
		"reason": meta.Reason,
		"elapsedMs": meta.ElapsedMs,
	},
})
```

Keep CLI file cleanup behavior by explicitly applying the cleanup package in
`cmd/soniox/main.go` after `transcribe.File`, using its existing CLI options.

- [ ] **Step 4: Run all Go tests and static checks**

Run: `rtk go test ./...`

Expected: PASS.

Run: `rtk go vet ./...`

Expected: no findings.

- [ ] **Step 5: Commit**

```bash
rtk git add internal/server/server.go internal/server/transcribe_test.go internal/transcribe/transcribe.go cmd/soniox/main.go
rtk git commit -m "feat(server): apply profiles to file transcription"
```

### Task 4: Verify Server Build

**Files:**
- No source changes expected

- [ ] **Step 1: Build the binary**

Run: `rtk go build ./cmd/soniox`

Expected: successful build.

- [ ] **Step 2: Review the final diff**

Run: `rtk git diff HEAD~3 --check`

Expected: no whitespace errors.

Run: `rtk git status --short`

Expected: clean worktree.
