package server

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/angelospk/soniox-core/internal/cleanup"
	"github.com/angelospk/soniox-core/internal/transcribe"
)

type trackingReader struct {
	*bytes.Reader
	reachedEOF bool
}

func (r *trackingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil {
		r.reachedEOF = true
	}
	return n, err
}

func multipartRequest(t *testing.T, filename string, content []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/transcribe", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v\nbody=%s", err, rec.Body.String())
	}
	return got
}

func writePref(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTranscribeKeepsExistingFields(t *testing.T) {
	s := &Server{
		KeyResolver: func(context.Context) (string, error) { return "key", nil },
		TranscribeFile: func(context.Context, string, transcribe.FileOptions) (transcribe.Result, error) {
			return transcribe.Result{Raw: "raw text", Cleaned: "raw text", Jobs: 2, Segments: 3, Duration: 42}, nil
		},
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, multipartRequest(t, "meeting.m4a", []byte("audio")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeResponse(t, rec)
	if got["raw"] != "raw text" || got["cleaned"] != "raw text" {
		t.Fatalf("missing legacy text fields: %#v", got)
	}
	if got["jobs"] != float64(2) || got["segments"] != float64(3) || got["duration_s"] != float64(42) {
		t.Fatalf("missing legacy stats: %#v", got)
	}
}

func TestCleanFileUsesActiveProfileLanguageVocabularyAndModels(t *testing.T) {
	prefs := t.TempDir()
	writePref(t, prefs, "active-profile", "notes")
	writePref(t, prefs, "output-language", "el")
	writePref(t, prefs, "vocabulary.txt", "# comment\nOpenCouncil\n")
	writePref(t, prefs, "cleanup-models", "model-a\nmodel-b")
	prompts := filepath.Join(prefs, "prompts")
	writePref(t, prompts, "notes.md", "# summarize: true\nCUSTOM NOTES")

	var got cleanup.Options
	s := &Server{
		Cleanup: true, GeminiKey: "gemini", PrefsDir: prefs,
		Clean: func(_ context.Context, _ string, _ string, opt cleanup.Options) cleanup.Result {
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

func TestTranscribeReturnsCleanupMetadata(t *testing.T) {
	prefs := t.TempDir()
	writePref(t, prefs, "active-profile", "default")
	writePref(t, prefs, "output-language", "el")

	s := &Server{
		KeyResolver: func(context.Context) (string, error) { return "key", nil },
		Cleanup:     true,
		GeminiKey:   "gemini",
		PrefsDir:    prefs,
		TranscribeFile: func(context.Context, string, transcribe.FileOptions) (transcribe.Result, error) {
			return transcribe.Result{Raw: "raw text", Cleaned: "raw text", Jobs: 1, Segments: 1, Duration: 3}, nil
		},
		Clean: func(context.Context, string, string, cleanup.Options) cleanup.Result {
			return cleanup.Result{
				Text: "polished text", Outcome: cleanup.Cleaned, Model: "model-a", ElapsedMs: 850,
			}
		},
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, multipartRequest(t, "meeting.m4a", []byte("audio")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeResponse(t, rec)
	if got["cleaned"] != "polished text" || got["profile"] != "default" || got["lang"] != "el" || got["model"] != "model-a" {
		t.Fatalf("response=%#v", got)
	}
	meta, ok := got["cleanup"].(map[string]any)
	if !ok || meta["outcome"] != "cleaned" || meta["reason"] != "" || meta["elapsedMs"] != float64(850) {
		t.Fatalf("cleanup=%#v", got["cleanup"])
	}
}

func TestTranscribeReturnsRawWhenAIDisabled(t *testing.T) {
	prefs := t.TempDir()
	writePref(t, prefs, "no-ai-cleanup", "")

	s := &Server{
		KeyResolver: func(context.Context) (string, error) { return "key", nil },
		Cleanup:     true,
		GeminiKey:   "gemini",
		PrefsDir:    prefs,
		TranscribeFile: func(context.Context, string, transcribe.FileOptions) (transcribe.Result, error) {
			return transcribe.Result{Raw: "raw text", Jobs: 1, Segments: 1, Duration: 3}, nil
		},
		Clean: func(context.Context, string, string, cleanup.Options) cleanup.Result {
			t.Fatal("cleanup must not run while AI correction is disabled")
			return cleanup.Result{}
		},
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, multipartRequest(t, "meeting.m4a", []byte("audio")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeResponse(t, rec)
	if got["cleaned"] != "raw text" {
		t.Fatalf("cleaned=%#v", got["cleaned"])
	}
	meta, ok := got["cleanup"].(map[string]any)
	if !ok || meta["outcome"] != "fallback_disabled" {
		t.Fatalf("cleanup=%#v", got["cleanup"])
	}
}

func TestTranscribeDrainsMalformedMultipartBeforeResponding(t *testing.T) {
	const boundary = "BROKEN"
	body := []byte("--BROKEN\r\nBroken header\r\n\r\n" + strings.Repeat("x", 1024) + "\r\n--BROKEN--\r\n")
	reader := &trackingReader{Reader: bytes.NewReader(body)}
	req := httptest.NewRequest(http.MethodPost, "/transcribe", reader)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	rec := httptest.NewRecorder()

	(&Server{}).Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !reader.reachedEOF {
		t.Fatal("handler returned before consuming the malformed request body")
	}
}
