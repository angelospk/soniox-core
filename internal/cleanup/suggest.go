package cleanup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Suggest is the low-confidence "did you mean" helper (Θ3): given a transcript and the
// words the recognizer was unsure about, it asks the model for up to a few likely
// intended alternatives per word. It is ADVISORY — deliberately separate from the
// cleanup rewrite pipeline (no Sanitize/accept/tooShort guards). It shares only the
// model transport with CleanResult.
//
// Items carry both a client-chosen Key (used verbatim in the response map, so the client
// and server never have to agree on a normalization rule) and the display Word the model
// reasons about. Alternatives are keyed by Key. A bad/empty model response degrades to an
// empty map with a non-nil error; callers treat that as "no suggestions", never fatal.

const suggestTimeout = 8 * time.Second

// SuggestItem is one uncertain word. Key is echoed back as the response map key; Word is
// what the model sees and reasons about.
type SuggestItem struct {
	Key  string `json:"key"`
	Word string `json:"word"`
}

// realSuggestModel hits Gemini with responseMimeType=application/json so the model is
// constrained to emit a JSON object (no strict schema — Gemini's schema support for
// dynamic-key maps is unreliable; the prompt describes the shape and parseSuggestions
// tolerates fences/prose as a fallback).
func realSuggestModel(apiKey string) modelFunc {
	return func(ctx context.Context, model, prompt string) (string, error) {
		cfg := genConfig(model)
		cfg["responseMimeType"] = "application/json"
		body, _ := json.Marshal(geminiReq{
			Contents:         []content{{Parts: []part{{Text: prompt}}}},
			GenerationConfig: cfg,
		})
		return callOnce(ctx, apiKey, geminiURL(model), body)
	}
}

func buildSuggestPrompt(text string, items []SuggestItem, lang string) string {
	var words []string
	for _, it := range items {
		words = append(words, it.Word)
	}
	var b strings.Builder
	b.WriteString("You are helping correct a dictation transcript. Below is the transcript and a list of words the speech recognizer was UNSURE about.\n\n")
	b.WriteString("For each uncertain word, suggest up to 3 words or short phrases the speaker most likely actually said, based on the surrounding context and how the word sounds. ")
	b.WriteString("Only suggest a genuinely plausible alternative; if the word is probably correct, return an empty list for it. Never invent unrelated words.\n")
	b.WriteString(langDirective(lang))
	b.WriteString("\nRespond ONLY with a JSON object mapping each uncertain word to an array of alternative strings, e.g. {\"word\": [\"alt1\",\"alt2\"]}. No prose, no markdown fences.\n\n")
	b.WriteString("Uncertain words: ")
	b.WriteString(strings.Join(words, ", "))
	b.WriteString("\n\nTranscript:\n")
	b.WriteString(text)
	return b.String()
}

// parseSuggestions tolerantly extracts the JSON object from a model reply: it strips
// markdown fences, then decodes the FIRST top-level object starting at the first '{' and
// IGNORES any trailing data. gemini-2.5-flash-lite sometimes appends a duplicated/partial
// fragment after a valid object (observed: `{...}\n": [...]`); decoding one value with a
// json.Decoder is robust to that, where first-'{'..last-'}' narrowing is not.
func parseSuggestions(raw string) (map[string][]string, error) {
	s := strings.TrimSpace(raw)
	// Strip ``` / ```json fences if present.
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimPrefix(s, "json")
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return nil, fmt.Errorf("no JSON object in model reply")
	}
	var m map[string][]string
	dec := json.NewDecoder(strings.NewReader(s[start:]))
	if err := dec.Decode(&m); err != nil { // reads one value; trailing junk ignored
		return nil, err
	}
	return m, nil
}

// sanitizeAlts trims, drops empties / control-char-bearing / word-equal entries, dedupes
// (accent-sensitive, case-insensitive), and caps the list. Phrases with spaces are kept.
func sanitizeAlts(alts []string, word string) []string {
	const maxAlts = 3
	wordKey := norm(word)
	seen := map[string]bool{}
	out := make([]string, 0, maxAlts)
	for _, a := range alts {
		t := strings.TrimSpace(a)
		if t == "" || hasControl(t) {
			continue
		}
		k := norm(t)
		if k == wordKey || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, t)
		if len(out) == maxAlts {
			break
		}
	}
	return out
}

// norm matches Corrections/Vocabulary normalization policy: case-insensitive, accent-
// sensitive. Local copy to avoid coupling to the unexported one elsewhere.
func norm(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// Suggest returns alternatives keyed by each item's Key. apiKey/text/items empty → empty
// map, nil error (nothing to do). A model or parse failure → empty map + error.
func Suggest(ctx context.Context, apiKey, text string, items []SuggestItem, lang string, caller modelFunc) (map[string][]string, string, error) {
	out := map[string][]string{}
	if apiKey == "" || strings.TrimSpace(text) == "" || len(items) == 0 {
		return out, "", nil
	}
	if caller == nil {
		caller = realSuggestModel(apiKey)
	}
	model := DefaultModel
	prompt := buildSuggestPrompt(text, items, lang)

	cctx, cancel := context.WithTimeout(ctx, suggestTimeout)
	defer cancel()
	raw, err := caller(cctx, model, prompt)
	if err != nil {
		return out, model, err
	}
	parsed, err := parseSuggestions(raw)
	if err != nil {
		return out, model, fmt.Errorf("%w (raw: %.300q)", err, raw)
	}
	// Index the reply by normalized text so we can look up by either the echoed key or the
	// display word, then iterate the REQUESTED items (not the reply) — this preserves every
	// item's Key, routes the same word's suggestions to each duplicate key, and naturally
	// drops hallucinated reply keys that were never requested.
	replyByNorm := map[string][]string{}
	for k, alts := range parsed {
		replyByNorm[norm(k)] = alts
	}
	for _, it := range items {
		alts, ok := parsed[it.Key]
		if !ok {
			alts, ok = replyByNorm[norm(it.Word)]
		}
		if !ok {
			continue
		}
		if clean := sanitizeAlts(alts, it.Word); len(clean) > 0 {
			out[it.Key] = clean
		}
	}
	return out, model, nil
}
