// Package server exposes the transcription core over HTTP + WebSocket so it can
// run as a service: stream live audio to /live, or POST a file to /transcribe.
package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/angelospk/soniox-core/internal/cleanup"
	"github.com/angelospk/soniox-core/internal/config"
	"github.com/angelospk/soniox-core/internal/soniox"
	"github.com/angelospk/soniox-core/internal/transcribe"
)

// Server holds shared config for the handlers.
type Server struct {
	KeyResolver func(context.Context) (string, error)
	GeminiKey   string
	Langs       []string
	Cleanup     bool
	Vocab       string
	// Reconnect forces a fresh Soniox key (re-runs the key command, rewrites the
	// cache) and returns a user-facing status. nil → /reconnect 503s.
	Reconnect func(context.Context) (config.ReconnectStatus, error)
	// Test seams; nil uses the production implementation.
	TranscribeFile func(context.Context, string, transcribe.FileOptions) (transcribe.Result, error)
	Clean          func(context.Context, string, string, cleanup.Options) cleanup.Result
	// LowConfThreshold flags STT words below this min-confidence to the client (0 → default).
	LowConfThreshold float64
	// PrefsDir is the shared prefs dir (~/.soniox-dictate). When set, /live reads
	// prompt profiles, the active profile, the trigger keyword, the AI on/off flag
	// and the vocabulary per request, so menu edits take effect without a restart.
	PrefsDir string
}

// Handler returns the HTTP mux with all routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/reconnect", s.handleReconnect)
	mux.HandleFunc("/live", s.handleLive)
	mux.HandleFunc("/transcribe", s.handleTranscribe)
	mux.HandleFunc("/clean", s.handleClean)
	mux.HandleFunc("/alternatives", s.handleAlternatives)
	mux.HandleFunc("/review", s.handleReview)
	return mux
}

// handleReconnect forces a fresh Soniox key so the next /live reconnects cleanly.
// It always answers JSON (config.ReconnectStatus) on success or a classified
// failure; a nil Reconnect seam means the feature is unavailable (503).
func (s *Server) handleReconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.Reconnect == nil {
		http.Error(w, "reconnect not available", http.StatusServiceUnavailable)
		return
	}
	status, err := s.Reconnect(r.Context())
	if err != nil {
		log.Printf("reconnect: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

var upgrader = websocket.Upgrader{
	CheckOrigin: checkOrigin,
}

// checkOrigin blocks cross-site WebSocket hijacking: native clients (Swift) send
// no Origin and are allowed; browser pages send an Origin, which must be local.
func checkOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}

// handleLive: client streams binary PCM (s16le mono 24kHz) frames; the server
// proxies to Soniox and streams JSON results back, finalizing when the client
// sends a text "done" frame or closes the socket.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	key, err := s.KeyResolver(r.Context())
	if err != nil {
		// kind:"key" lets the client auto-heal (force a fresh Soniox key) instead
		// of just surfacing the failure — this is the "key expired / can't refresh"
		// path a manual Reconnect would also fix.
		writeJSON(conn, nil, map[string]any{"type": "error", "kind": "key", "message": err.Error()})
		log.Printf("live key error: %v", err)
		return
	}

	st := &liveStats{start: time.Now()}
	var writeMu sync.Mutex
	audio := make(chan []byte, 64)

	// reader: pump client frames into the audio channel
	go func() {
		defer close(audio)
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				st.clientGone(err)
				return
			}
			switch mt {
			case websocket.BinaryMessage:
				st.addAudio(data)
				audio <- data
			case websocket.TextMessage:
				if string(data) == "done" {
					return
				}
			}
		}
	}()

	langs := s.Langs
	if q := r.URL.Query()["lang"]; len(q) > 0 {
		langs = q
	}

	full, err := soniox.Stream(r.Context(),
		// APIKeyFunc (dynamic) so a mid-stream reconnect re-reads the freshest temp
		// key from the cache rather than retrying with the one resolved at connect.
		// Soniox only checks the key at the handshake, so a long recording that
		// started valid keeps streaming even past the ~1h key expiry; this only
		// matters if a drop forces a reconnect after the key has rotated. The
		// upfront KeyResolver above stays for fail-fast (clear error, no key at all).
		soniox.Options{
			APIKeyFunc:        func() (string, error) { return s.KeyResolver(r.Context()) },
			APIKey:            key,
			Langs:             langs,
			EndpointDetection: false,
			LowConfThreshold:  s.LowConfThreshold,
		},
		audio,
		soniox.Callbacks{
			OnFinal: func(t string, low []soniox.LowConfRange) {
				st.inc(&st.finals)
				m := map[string]any{"type": "final", "text": t}
				if len(low) > 0 { // new field; old clients ignore it (no crash, no highlight)
					m["low_conf_ranges"] = low
				}
				writeJSON(conn, &writeMu, m)
			},
			OnPartial: func(t string) {
				st.partial(t)
				writeJSON(conn, &writeMu, map[string]any{"type": "partial", "text": t})
			},
			OnReconnect: func(n int) {
				st.inc(&st.reconnects)
				writeJSON(conn, &writeMu, map[string]any{"type": "reconnecting", "attempt": n})
			},
		})
	log.Print(st.line(len(strings.TrimSpace(full)), err))
	if err != nil {
		writeJSON(conn, &writeMu, map[string]any{"type": "error", "message": err.Error()})
		return
	}

	// server-side cleanup so every client gets the same polished text. The voice
	// command suffix (if any) is always stripped — even with AI off — so the
	// literal command never lands in the output.
	// Panel mode opts out of server-side auto-clean (autoclean=0) so the client owns
	// prompting on demand; quick mode omits the param and keeps auto-clean.
	var cleaned string
	var meta cleanupMeta
	if r.URL.Query().Get("autoclean") == "0" {
		cleaned, meta = s.skipClean(full)
	} else {
		cleaned, meta = s.cleanLive(r.Context(), conn, &writeMu, full)
	}
	writeJSON(conn, &writeMu, map[string]any{
		"type": "done", "text": cleaned, "raw": full,
		"profile": meta.Profile, "lang": meta.Lang, "model": meta.Model, "adhoc": meta.Adhoc,
		"send": meta.Send,
		"cleanup": map[string]any{
			"outcome": string(meta.Outcome), "reason": meta.Reason, "elapsedMs": meta.ElapsedMs,
		},
	})
}

// handleTranscribe: POST multipart "file" -> JSON {raw, cleaned, ...}.
func (s *Server) handleTranscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST a multipart 'file'", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 512<<20)       // cap upload at 512 MB
	if err := r.ParseMultipartForm(64 << 20); err != nil { // #nosec G120 -- bounded above by MaxBytesReader(512MB)
		// Drain the bounded body before responding. URLSession upload tasks may
		// otherwise receive the early error while still writing and never deliver
		// their completion callback after the server resets the connection.
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing 'file'", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// sanitize: never let a client-supplied filename influence the path
	safe := filepath.Base(hdr.Filename)
	if safe == "." || safe == ".." || safe == string(filepath.Separator) {
		safe = "upload"
	}
	tmp, err := os.CreateTemp("", "soniox-*-"+safe)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, file); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmp.Close()

	key, err := s.KeyResolver(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	res, err := s.transcribeFile(r.Context(), tmp.Name(), transcribe.FileOptions{
		Key: key, Langs: s.Langs,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	cleaned, meta := s.cleanText(r.Context(), res.Raw, false)
	res.Cleaned = cleaned
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"raw": res.Raw, "cleaned": res.Cleaned,
		"jobs": res.Jobs, "segments": res.Segments, "duration_s": res.Duration,
		"profile": meta.Profile, "lang": meta.Lang, "model": meta.Model,
		"cleanup": map[string]any{
			"outcome": string(meta.Outcome), "reason": meta.Reason, "elapsedMs": meta.ElapsedMs,
		},
	})
}

func (s *Server) transcribeFile(ctx context.Context, path string, opt transcribe.FileOptions) (transcribe.Result, error) {
	if s.TranscribeFile != nil {
		return s.TranscribeFile(ctx, path, opt)
	}
	return transcribe.File(ctx, path, opt)
}

func (s *Server) cleanResult(ctx context.Context, text string, opt cleanup.Options) cleanup.Result {
	if s.Clean != nil {
		return s.Clean(ctx, s.GeminiKey, text, opt)
	}
	return cleanup.CleanResult(ctx, s.GeminiKey, text, opt)
}

// handleClean runs cleanup on demand: a finalized transcript + an explicit profile,
// returning the cleaned text. Stateless — the panel uses it to apply/compare prompts
// after stopping, re-running from the same base text each time.
func (s *Server) handleClean(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Text    string `json:"text"`
		Profile string `json:"profile"`
		Lang    string `json:"lang"`
		Adhoc   string `json:"adhoc"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	cleaned, meta := s.cleanExplicit(r.Context(), req.Text, req.Profile, req.Adhoc, req.Lang)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"cleaned": cleaned, "profile": meta.Profile, "model": meta.Model,
		"outcome": string(meta.Outcome), "reason": meta.Reason, "elapsedMs": meta.ElapsedMs,
	})
}

// handleAlternatives runs the low-confidence "did you mean" helper (Θ3): given a finalized
// transcript + the uncertain words, returns up to a few likely intended alternatives per
// word. UNLIKE /clean, this fires AUTOMATICALLY on stop, so it HONORS the no-ai-cleanup
// flag (a user who opted out of automatic Gemini calls shouldn't have their transcript
// sent here either). Advisory: any error → empty map, never a non-200.
func (s *Server) handleAlternatives(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Text  string                `json:"text"`
		Items []cleanup.SuggestItem `json:"items"`
		Lang  string                `json:"lang"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	writeAlts := func(alts map[string][]string, model string, ms int64) {
		if alts == nil {
			alts = map[string][]string{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"alternatives": alts, "model": model, "elapsedMs": ms})
	}
	aiOff := s.PrefsDir != "" && fileExists(filepath.Join(s.PrefsDir, "no-ai-cleanup"))
	items := dedupeItems(req.Items)
	if aiOff || !s.Cleanup || s.GeminiKey == "" || len(items) == 0 {
		writeAlts(nil, "", 0)
		return
	}
	start := time.Now()
	alts, model, err := cleanup.Suggest(r.Context(), s.GeminiKey, req.Text, items, req.Lang, nil)
	if err != nil {
		log.Printf("alternatives: %v", err)
		writeAlts(nil, "", time.Since(start).Milliseconds())
		return
	}
	writeAlts(alts, model, time.Since(start).Milliseconds())
}

// handleReview runs the post-dictation proof-reading pass: given the finalized transcript
// (plus the words the recognizer was unsure about and the user's own term list), it returns
// a short list of proposed edits the client offers one at a time.
//
// It is gated by its OWN flag (no-review), NOT by no-ai-cleanup. The two are deliberately
// independent: this user runs with LLM cleanup off and still wants the review pass, so
// folding them together would make the feature unreachable for exactly its intended user.
// Turning off AI cleanup therefore no longer means "no automatic Gemini traffic" — set
// no-review as well for that.
//
// Advisory, like /alternatives: every failure returns 200 with an empty list, so the client
// never has to distinguish "nothing to report" from "the model was busy". Only a malformed
// request is a 4xx.
func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SessionID  string                    `json:"sessionId"`
		Revision   int                       `json:"revision"`
		Text       string                    `json:"text"`
		Lang       string                    `json:"lang"`
		Uncertain  []cleanup.ReviewUncertain `json:"uncertain"`
		Vocabulary []string                  `json:"vocabulary"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	// The client echoes its own session + revision back so a late reply can be matched
	// against the panel's current state and discarded if it has moved on.
	write := func(findings []cleanup.Finding, model string, ms int64) {
		if findings == nil {
			findings = []cleanup.Finding{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessionId": req.SessionID, "revision": req.Revision,
			"findings": findings, "model": model, "elapsedMs": ms,
		})
	}
	text, unc, vocab, err := cleanup.ClampReviewInput(req.Text, req.Uncertain, req.Vocabulary)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	off := s.PrefsDir != "" && fileExists(filepath.Join(s.PrefsDir, "no-review"))
	if off || !s.Cleanup || s.GeminiKey == "" || strings.TrimSpace(text) == "" {
		write(nil, "", 0)
		return
	}
	start := time.Now()
	findings, model, err := cleanup.Review(r.Context(), s.GeminiKey, text, req.Lang, unc, vocab,
		s.modelChain(), nil)
	if err != nil {
		log.Printf("review: %v", err)
		write(nil, "", time.Since(start).Milliseconds())
		return
	}
	write(findings, model, time.Since(start).Milliseconds())
}

// dedupeItems trims, drops empty key/word, dedupes by key, and caps the count so a
// runaway client can't fan out an unbounded prompt.
func dedupeItems(in []cleanup.SuggestItem) []cleanup.SuggestItem {
	const maxItems = 40
	seen := map[string]bool{}
	out := make([]cleanup.SuggestItem, 0, len(in))
	for _, it := range in {
		key := strings.TrimSpace(it.Key)
		word := strings.TrimSpace(it.Word)
		if key == "" || word == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, cleanup.SuggestItem{Key: key, Word: word})
		if len(out) == maxItems {
			break
		}
	}
	return out
}

// cleanLive resolves the active profile + voice command from the raw transcript,
// strips the command, and (unless AI is off) runs Gemini cleanup with the
// selected profile, emitting a "cleaning" event first so the client can show it.
func (s *Server) cleanLive(ctx context.Context, conn *websocket.Conn, mu *sync.Mutex, full string) (string, cleanupMeta) {
	if s.cleanupWillRun(full) {
		writeJSON(conn, mu, map[string]any{"type": "cleaning"})
	}
	text, meta := s.cleanText(ctx, full, true)
	if meta.Outcome == cleanup.FallbackError || meta.Outcome == cleanup.FallbackShort {
		writeJSON(conn, mu, map[string]any{"type": "cleanup_failed", "reason": string(meta.Outcome), "detail": meta.Reason})
	}
	return text, meta
}

// skipClean is the panel-mode path (/live?autoclean=0): server-side Gemini cleanup is
// off, but the spoken voice command suffix is still stripped — via the same
// profile/command resolution cleanLive uses — so the literal keyword never lands in
// the output. Returns the full (uncleaned) transcript minus the command, marked Skipped.
func (s *Server) skipClean(full string) (string, cleanupMeta) {
	text, name, adHoc, lang, send := s.resolveLiveCommand(full, true)
	return text, cleanupMeta{Profile: name, Lang: lang, Outcome: cleanup.Skipped, Adhoc: adHoc != "", Send: send}
}

func (s *Server) cleanupWillRun(text string) bool {
	if strings.TrimSpace(text) == "" || !s.Cleanup || s.GeminiKey == "" {
		return false
	}
	return s.PrefsDir == "" || !fileExists(filepath.Join(s.PrefsDir, "no-ai-cleanup"))
}

// cleanText applies the shared cleanup preferences. Live dictation may select a
// profile with a trailing voice command; file transcription always uses the
// active profile for the complete file.
func (s *Server) cleanText(ctx context.Context, full string, allowVoiceCommand bool) (string, cleanupMeta) {
	if strings.TrimSpace(full) == "" {
		return full, cleanupMeta{Outcome: cleanup.RawTooShort}
	}

	// Legacy path: no shared prefs dir configured (e.g. tests) — keep old behavior.
	if s.PrefsDir == "" {
		if s.Cleanup && s.GeminiKey != "" {
			r := s.cleanResult(ctx, full, cleanup.Options{Vocab: s.Vocab})
			m := cleanupMeta{Profile: "default", Model: r.Model, Outcome: r.Outcome, Reason: r.Reason, ElapsedMs: r.ElapsedMs}
			logCleanup(m, r)
			return r.Text, m
		}
		return full, cleanupMeta{Profile: "default", Outcome: cleanup.FallbackDisabled}
	}

	text, name, adHoc, lang, send := s.resolveLiveCommand(full, allowVoiceCommand)
	// Honor the no-ai-cleanup preference on the AUTOMATIC path only (explicit /clean
	// ignores it — see cleanExplicit). Keeps the spoken-command strip + returns raw.
	if s.PrefsDir != "" && fileExists(filepath.Join(s.PrefsDir, "no-ai-cleanup")) {
		return text, cleanupMeta{Profile: name, Lang: lang, Outcome: cleanup.FallbackDisabled, Adhoc: adHoc != "", Send: send}
	}
	out, meta := s.cleanExplicit(ctx, text, name, adHoc, lang)
	meta.Send = send
	return out, meta
}

// resolveLiveCommand loads the active profile, trigger keyword, and output language
// from PrefsDir and (when allowVoiceCommand) strips a trailing spoken voice command
// from the transcript, resolving any profile switch / ad-hoc instruction it carries.
// Shared by the auto-clean path (cleanText) and the panel-mode skip path (skipClean)
// so the command is stripped identically whether or not Gemini cleanup runs. With no
// PrefsDir (legacy/tests) it returns the text unchanged under the "default" profile.
func (s *Server) resolveLiveCommand(full string, allowVoiceCommand bool) (text, name, adHoc, lang string, send bool) {
	if s.PrefsDir == "" {
		return full, "default", "", "", false
	}
	profiles, _ := cleanup.LoadProfiles(filepath.Join(s.PrefsDir, "prompts"))
	keyword := readTrimmed(filepath.Join(s.PrefsDir, "trigger-keyword"))
	active := readTrimmed(filepath.Join(s.PrefsDir, "active-profile"))
	if active == "" {
		active = "default"
	}
	active = filepath.Base(active) // never let the file content escape the dir

	text, name = full, active
	if allowVoiceCommand {
		if prof, ad, stripped, ok, snd := cleanup.ParseCommandSend(full, keyword, profiles); ok {
			if prof != "" {
				name = prof
			}
			text, adHoc, send = stripped, ad, snd
		}
	}
	lang = readTrimmed(filepath.Join(s.PrefsDir, "output-language"))
	return text, name, adHoc, lang, send
}

// cleanExplicit runs cleanup with an already-resolved profile name + optional ad-hoc
// instruction. Request-scoped: loads profiles into locals and shares no state. Used by
// the /live path (after voice-command resolution) and the on-demand /clean endpoint.
// An unknown profile falls back to "default" and reports reason "unknown_profile_fallback".
func (s *Server) cleanExplicit(ctx context.Context, text, name, adHoc, lang string) (string, cleanupMeta) {
	profiles, _ := cleanup.LoadProfiles(filepath.Join(s.PrefsDir, "prompts"))
	name = filepath.Base(name) // never let the requested name escape the prompts dir
	if name == "" {
		name = "default"
	}
	reason := ""
	if _, ok := profiles[name]; !ok && name != "default" {
		name, reason = "default", "unknown_profile_fallback"
	}
	// NOTE: the no-ai-cleanup flag is intentionally NOT checked here. It disables only
	// AUTOMATIC cleanup (the /live path checks it). An explicit /clean — the user
	// picking a prompt — always runs, gated only by hard limits (cleanup off / no key).
	if !s.Cleanup || s.GeminiKey == "" || strings.TrimSpace(text) == "" {
		return text, cleanupMeta{Profile: name, Lang: lang, Outcome: cleanup.FallbackDisabled, Reason: reason, Adhoc: adHoc != ""}
	}
	prof := profiles[name]
	// Bypass the length guard for a summarizing profile that actually has a body (an
	// empty body falls back to the conservative default, where a big shrink would be
	// wrong), OR when an ad-hoc instruction is present — it may legitimately reshape or
	// shorten the text, so honor length changes.
	allowShrink := (prof.Summarize && strings.TrimSpace(prof.Body) != "") || adHoc != ""
	r := s.cleanResult(ctx, text, cleanup.Options{
		Vocab:       s.readVocab(),
		Body:        cleanup.ProfileBody(profiles, name),
		AdHoc:       adHoc,
		Lang:        lang,
		AllowShrink: allowShrink,
		Models:      s.modelChain(),
	})
	m := cleanupMeta{Profile: name, Lang: lang, Model: r.Model, Outcome: r.Outcome, Reason: firstNonEmpty(reason, r.Reason), ElapsedMs: r.ElapsedMs, Adhoc: adHoc != ""}
	logCleanup(m, r)
	return r.Text, m
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// cleanupMeta is the per-session cleanup outcome surfaced to the client (in the
// "done" event) and to the logs.
type cleanupMeta struct {
	Profile   string
	Lang      string
	Model     string
	Outcome   cleanup.Outcome
	Reason    string
	ElapsedMs int64
	Adhoc     bool
	Send      bool // spoken "τέλος μηνύματος": client presses Return after pasting
}

// logCleanup writes one structured line per cleanup. Reason is already a sanitized
// category (no raw API bodies).
func logCleanup(m cleanupMeta, r cleanup.Result) {
	log.Printf("cleanup profile=%s lang=%s model=%s in=%d out=%d ratio=%.2f outcome=%s reason=%s attempts=%d elapsedMs=%d",
		m.Profile, m.Lang, m.Model, r.InLen, r.OutLen, r.Ratio, r.Outcome, r.Reason, r.Attempts, r.ElapsedMs)
}

// readVocab reads the live vocabulary file, falling back to the startup copy.
func (s *Server) readVocab() string {
	b, err := os.ReadFile(filepath.Join(s.PrefsDir, "vocabulary.txt")) // #nosec G304 -- fixed prefs path
	if err != nil {
		return s.Vocab
	}
	var lines []string
	for _, ln := range strings.Split(string(b), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" && !strings.HasPrefix(ln, "#") {
			lines = append(lines, ln)
		}
	}
	return strings.Join(lines, "\n")
}

// modelChain returns the cleanup model fallback chain. It reads an optional
// override at ~/.soniox-dictate/cleanup-models (one model per line, or comma/space
// separated; '#' lines ignored) so the chain can change with no rebuild; an empty
// or missing file falls back to cleanup.DefaultModels.
func (s *Server) modelChain() []string {
	if s.PrefsDir == "" {
		return nil // legacy/test path → cleanup uses DefaultModels
	}
	return cleanup.ReadModelChain(filepath.Join(s.PrefsDir, "cleanup-models"))
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func readTrimmed(p string) string {
	b, err := os.ReadFile(p) // #nosec G304 -- fixed prefs path
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeJSON(conn *websocket.Conn, mu *sync.Mutex, v any) {
	b, _ := json.Marshal(v)
	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	conn.WriteMessage(websocket.TextMessage, b)
}
