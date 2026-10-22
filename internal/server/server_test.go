package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/angelospk/soniox-core/internal/audio"
	"github.com/angelospk/soniox-core/internal/cleanup"
	"github.com/angelospk/soniox-core/internal/soniox"
)

// Live /live WebSocket test against the real Soniox API.
// Skipped unless SONIOX_TEMP_KEY (or SONIOX_API_KEY) and TEST_AUDIO are set.
func TestLiveIntegration(t *testing.T) {
	key := os.Getenv("SONIOX_TEMP_KEY")
	if key == "" {
		key = os.Getenv("SONIOX_API_KEY")
	}
	audioPath := os.Getenv("TEST_AUDIO")
	if key == "" || audioPath == "" {
		t.Skip("set SONIOX_TEMP_KEY and TEST_AUDIO for the live /live test")
	}

	ctx := context.Background()
	pcm, err := audio.DecodePCM(ctx, audioPath, 0, 15) // first 15s
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	srv := &Server{
		KeyResolver: func(context.Context) (string, error) { return key, nil },
		Langs:       []string{"el", "en"},
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/live"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// reader: collect until a "done" message
	type msg struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	doneText := make(chan string, 1)
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				doneText <- ""
				return
			}
			var m msg
			if json.Unmarshal(data, &m) == nil && m.Type == "done" {
				doneText <- m.Text
				return
			}
		}
	}()

	// sender: paced PCM frames, then "done"
	cb := soniox.ChunkBytes(100)
	for i := 0; i < len(pcm); i += cb {
		end := i + cb
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, pcm[i:end]); err != nil {
			t.Fatalf("write: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	conn.WriteMessage(websocket.TextMessage, []byte("done"))

	select {
	case text := <-doneText:
		if strings.TrimSpace(text) == "" {
			t.Fatal("got empty transcript from /live")
		}
		t.Logf("/live transcript: %s", text)
	case <-time.After(60 * time.Second):
		t.Fatal("timeout waiting for /live result")
	}
}

// TestSkipCleanStripsCommand covers the panel mode (/live?autoclean=0) path: Gemini
// cleanup is skipped, but the spoken voice command suffix must still be stripped so the
// literal keyword never leaks into done.text.
func TestSkipCleanStripsCommand(t *testing.T) {
	dir := t.TempDir() // LoadProfiles seeds built-ins (default, message, …) on first read
	s := &Server{PrefsDir: dir}

	text, meta := s.skipClean("γεια σου ραντεβού αύριο, εντολή μήνυμα.")

	if text != "γεια σου ραντεβού αύριο" {
		t.Fatalf("command not stripped: got %q", text)
	}
	if meta.Outcome != cleanup.Skipped {
		t.Fatalf("outcome = %q, want %q", meta.Outcome, cleanup.Skipped)
	}
	if meta.Profile != "message" {
		t.Fatalf("profile = %q, want message", meta.Profile)
	}
}

// postClean drives handleClean with a JSON body and returns status + decoded response.
func postClean(t *testing.T, s *Server, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/clean", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleClean(rec, req)
	var out map[string]any
	if rec.Code == http.StatusOK {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func TestHandleClean(t *testing.T) {
	dir := t.TempDir() // LoadProfiles seeds built-ins (default, message, notes…) here
	// No GeminiKey → cleanExplicit passes the text through with fallback_disabled, which
	// exercises the profile-resolution logic without calling the LLM.
	s := &Server{PrefsDir: dir, Cleanup: true}

	t.Run("known profile passthrough", func(t *testing.T) {
		code, out := postClean(t, s, `{"text":"hello world","profile":"message"}`)
		if code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		if out["cleaned"] != "hello world" || out["profile"] != "message" || out["outcome"] != "fallback_disabled" {
			t.Fatalf("got %+v", out)
		}
		if out["reason"] != "" {
			t.Fatalf("unexpected reason %v", out["reason"])
		}
	})

	t.Run("unknown profile falls back to default", func(t *testing.T) {
		_, out := postClean(t, s, `{"text":"hi","profile":"does-not-exist"}`)
		if out["profile"] != "default" || out["reason"] != "unknown_profile_fallback" {
			t.Fatalf("got %+v", out)
		}
	})

	t.Run("bad json -> 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/clean", strings.NewReader("{not json"))
		rec := httptest.NewRecorder()
		s.handleClean(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d", rec.Code)
		}
	})

	t.Run("GET -> 405", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/clean", nil)
		rec := httptest.NewRecorder()
		s.handleClean(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status %d", rec.Code)
		}
	})
}

func TestHandleAlternatives(t *testing.T) {
	// No GeminiKey → endpoint returns an empty map without calling the model, which
	// exercises the request validation + JSON shape without network.
	s := &Server{Cleanup: true}

	post := func(body string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/alternatives", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleAlternatives(rec, req)
		var out map[string]any
		if rec.Code == http.StatusOK {
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
		}
		return rec.Code, out
	}

	t.Run("no key → empty alternatives", func(t *testing.T) {
		code, out := post(`{"text":"rite there","items":[{"key":"rite","word":"rite"}],"lang":"en"}`)
		if code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		alts, ok := out["alternatives"].(map[string]any)
		if !ok || len(alts) != 0 {
			t.Fatalf("expected empty alternatives, got %+v", out["alternatives"])
		}
	})

	t.Run("bad json → 400", func(t *testing.T) {
		if code, _ := post("{nope"); code != http.StatusBadRequest {
			t.Fatalf("status %d", code)
		}
	})

	t.Run("GET → 405", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/alternatives", nil)
		rec := httptest.NewRecorder()
		s.handleAlternatives(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status %d", rec.Code)
		}
	})

	t.Run("no-ai-cleanup honored", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "no-ai-cleanup"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		s2 := &Server{Cleanup: true, GeminiKey: "x", PrefsDir: dir}
		req := httptest.NewRequest(http.MethodPost, "/alternatives",
			strings.NewReader(`{"text":"rite","items":[{"key":"rite","word":"rite"}]}`))
		rec := httptest.NewRecorder()
		s2.handleAlternatives(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response: %v; body=%q", err, rec.Body.String())
		}
		if alts, _ := out["alternatives"].(map[string]any); len(alts) != 0 {
			t.Fatalf("expected empty (flag set), got %+v", out["alternatives"])
		}
	})
}

func TestHandleReview(t *testing.T) {
	// No GeminiKey → the endpoint short-circuits without a network call, which exercises
	// the request validation, the flag gating and the response shape.
	post := func(s *Server, body string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/review", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleReview(rec, req)
		var out map[string]any
		if rec.Code == http.StatusOK {
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
		}
		return rec.Code, out
	}

	t.Run("no key → empty findings, still 200", func(t *testing.T) {
		code, out := post(&Server{Cleanup: true}, `{"sessionId":"s1","revision":3,"text":"ναι","lang":"el"}`)
		if code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		f, ok := out["findings"].([]any)
		if !ok || len(f) != 0 {
			t.Fatalf("want empty findings array, got %+v", out["findings"])
		}
	})

	// The client matches a late reply against its own state; dropping the echo would make
	// every reply look current.
	t.Run("echoes session and revision", func(t *testing.T) {
		_, out := post(&Server{Cleanup: true}, `{"sessionId":"s1","revision":3,"text":"ναι"}`)
		if out["sessionId"] != "s1" {
			t.Fatalf("sessionId not echoed: %+v", out["sessionId"])
		}
		if rev, _ := out["revision"].(float64); int(rev) != 3 {
			t.Fatalf("revision not echoed: %+v", out["revision"])
		}
	})

	t.Run("oversized text is a 400, not a silent empty", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{"text": strings.Repeat("x", cleanup.MaxReviewTextBytes+1)})
		code, _ := post(&Server{Cleanup: true}, string(body))
		if code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", code)
		}
	})

	t.Run("no-review flag disables it", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "no-review"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		s := &Server{Cleanup: true, GeminiKey: "k", PrefsDir: dir}
		code, out := post(s, `{"text":"ναι"}`)
		if code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		if f, _ := out["findings"].([]any); len(f) != 0 {
			t.Fatalf("flag ignored: %+v", f)
		}
	})

	t.Run("GET is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/review", nil)
		rec := httptest.NewRecorder()
		(&Server{Cleanup: true}).handleReview(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status %d", rec.Code)
		}
	})
}

// TestSkipCleanSendCommand: "τέλος μηνύματος" is stripped and flagged so the client
// presses Return after the paste.
func TestSkipCleanSendCommand(t *testing.T) {
	s := &Server{PrefsDir: t.TempDir()}
	text, meta := s.skipClean("τα λέμε αύριο, τέλος μηνύματος.")
	if text != "τα λέμε αύριο" || !meta.Send {
		t.Fatalf("got text=%q send=%v", text, meta.Send)
	}
	if _, meta := s.skipClean("τα λέμε αύριο"); meta.Send {
		t.Fatal("send set without command")
	}
}
