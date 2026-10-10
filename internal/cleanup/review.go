package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Review is the post-dictation "does this actually make sense?" pass. It is ADVISORY and
// deliberately separate from CleanResult: it never rewrites the transcript, it returns a
// short list of proposed edits the user accepts one at a time, and its findings double as
// teaching material (a mis-hearing becomes a vocabulary entry, a verbal tic becomes a
// deletion rule) so the same problem costs nothing to fix next time.
//
// Scope is the WHOLE transcript. The uncertain-word list is a focus hint — the words the
// recognizer already flagged — not a restriction, because the redundancy findings the user
// cares about ("τέλος πάντων") are never the words Soniox was unsure of.
//
// The model is not trusted with offsets. It quotes an exact substring; the server locates
// it. See locate().

const (
	// One deadline for the whole call, with a per-attempt budget inside it, so a stalled
	// first model still leaves room for a second rather than eating the entire window.
	reviewTotalDeadline = 6 * time.Second
	reviewPerAttempt    = 2500 * time.Millisecond

	maxReviewFindings = 6
	maxWhyRunes       = 120
	maxToRunes        = 200

	// Request limits. A transcript far past this is a file transcription, not a dictation,
	// and reviewing it would blow the latency budget for no benefit.
	MaxReviewTextBytes = 32 << 10
	MaxReviewUncertain = 64
	MaxReviewVocab     = 500
)

// Finding kinds. misheard/nonsense propose a replacement; filler/redundant propose a
// deletion. The kind drives which buttons the client offers, so an unknown kind is
// dropped rather than guessed at.
const (
	KindMisheard  = "misheard"
	KindFiller    = "filler"
	KindRedundant = "redundant"
	KindNonsense  = "nonsense"
)

func isDeleteKind(k string) bool { return k == KindFiller || k == KindRedundant }

// ReviewMode selects how aggressive the proof-reader is allowed to be.
//
// The zero value is the legacy behaviour on purpose: dictation-bar has been
// calling Review for months and must keep getting the same four kinds without
// touching a line of its own code. Meaning-only is the additive P16 mode (D30)
// for Brain's voice capture, where the contract with the speaker is that the
// machine may fix what it MIS-HEARD and nothing else (D21) — the words are his,
// and a "better" phrasing he did not choose is a silent meaning change.
type ReviewMode string

const (
	ReviewModeDefault     ReviewMode = ""
	ReviewModeMeaningOnly ReviewMode = "meaning-only"
)

// ReviewOptions carries the additive knobs so a future one does not change every
// call site again.
type ReviewOptions struct {
	Mode ReviewMode
}

func ValidReviewMode(mode ReviewMode) bool {
	return mode == ReviewModeDefault || mode == ReviewModeMeaningOnly
}

// allowsKind reports whether a finding kind may survive validation in this mode.
// Meaning-only enforces the restriction server-side rather than trusting the
// prompt: a model that ignores an instruction must not be able to delete the
// speaker's words anyway.
func (m ReviewMode) allowsKind(kind string) bool {
	if m == ReviewModeMeaningOnly {
		return kind == KindMisheard || kind == KindNonsense
	}
	return true
}

// ReviewUncertain is one word the recognizer flagged, with its byte range in the
// submitted text. Used to disambiguate which occurrence of a repeated word a finding
// refers to.
type ReviewUncertain struct {
	Key   string `json:"key"`
	Word  string `json:"word"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// Finding is one proposed edit. Start/End are UTF-8 byte offsets into the exact text the
// client submitted — computed by the server, never by the model.
type Finding struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	From  string `json:"from"`
	To    string `json:"to"`
	Kind  string `json:"kind"`
	Why   string `json:"why"`
}

// rawFinding is the model's shape: a quoted substring and an optional key, no offsets.
type rawFinding struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
	Why  string `json:"why"`
	Key  string `json:"key"`
}

func buildReviewPrompt(text, lang string, unc []ReviewUncertain, vocab []string, mode ReviewMode) string {
	var b strings.Builder
	b.WriteString("You are proof-reading a raw speech-to-text transcript for the person who dictated it. ")
	b.WriteString("Report only problems worth their attention — at most 6, fewer is better, none is a valid answer.\n\n")
	b.WriteString("Report these kinds:\n")
	b.WriteString("- \"misheard\": the recognizer clearly heard the wrong word. Give the word they actually said.\n")
	b.WriteString("- \"nonsense\": a phrase that cannot be what they meant. Give a reading that fits the context.\n")
	if mode == ReviewModeMeaningOnly {
		b.WriteString("\nReport NOTHING else. Specifically, you MUST NOT report or propose:\n")
		b.WriteString("- grammar, spelling-as-style, punctuation or capitalisation improvements\n")
		b.WriteString("- fillers, hesitations, verbal tics, repetitions or self-interruptions\n")
		b.WriteString("- redundant or wordy phrasing, summarisation, shortening or tightening\n")
		b.WriteString("- vocabulary, tone or register changes, however small\n")
		b.WriteString("These are the speaker's own words. Only fix what the recognizer got WRONG.\n")
		b.WriteString("Every finding MUST have a non-empty \"to\": there are no deletions in this mode.\n\n")
	} else {
		b.WriteString("- \"filler\": a hesitation token that survived cleanup.\n")
		b.WriteString("- \"redundant\": a verbal tic or self-interruption that carries no meaning and reads better gone.\n\n")
	}
	b.WriteString("Work through the WHOLE transcript from start to finish. Do not stop at the first problem, ")
	b.WriteString("and do not limit yourself to the flagged words — spelling mistakes and verbal tics are ")
	b.WriteString("usually words the recognizer was perfectly confident about.\n\n")
	b.WriteString("Rules:\n")
	b.WriteString("- \"from\" MUST be copied character-for-character from the transcript. Never paraphrase it.\n")
	b.WriteString("- For \"misheard\" and \"nonsense\", \"to\" is the replacement text.\n")
	if mode != ReviewModeMeaningOnly {
		b.WriteString("- For \"filler\" and \"redundant\", \"to\" MUST be the empty string \"\".\n")
	}
	b.WriteString("- Keep the grammatical form of the word you are replacing: same case, number and tense. ")
	b.WriteString("A name listed in the nominative must still be replaced in whatever case the sentence uses.\n")
	b.WriteString("- Never change the speaker's meaning, tone, vocabulary or register. Do not polish their style.\n")
	b.WriteString("- Never translate. The transcript may mix Greek and English; leave each word in its language.\n")
	b.WriteString("- \"why\" is one short clause, in the transcript's language.\n")
	b.WriteString(langDirective(lang))
	if len(unc) > 0 {
		words := make([]string, 0, len(unc))
		for _, u := range unc {
			words = append(words, u.Word)
		}
		b.WriteString("\nThe recognizer was UNSURE about these words — check them first, but do not stop there: ")
		b.WriteString(strings.Join(words, ", "))
		b.WriteString("\n")
	}
	if len(vocab) > 0 {
		b.WriteString("\nThe speaker's own terms and names, spelled the way they want them. Any word in the ")
		b.WriteString("transcript that is a near-match of one of these — one or two letters out, a missing ")
		b.WriteString("double consonant, a different accent — is a mis-hearing and MUST be reported. Never ")
		b.WriteString("insert one of these terms where it was not said:\n")
		b.WriteString(strings.Join(vocab, ", "))
		b.WriteString("\n")
	}
	b.WriteString("\nRespond ONLY with a JSON object of this shape. No prose, no markdown fences.\n")
	b.WriteString(`{"findings":[{"from":"...","to":"...","kind":"misheard","why":"...","key":""}]}` + "\n")
	b.WriteString("An empty list is correct when the transcript is fine: {\"findings\":[]}\n")
	b.WriteString("\nTranscript:\n")
	b.WriteString(text)
	return b.String()
}

// parseReview extracts the findings array from a model reply, tolerating markdown fences
// and trailing junk the same way parseSuggestions does (the lite models occasionally
// append a partial duplicate after a valid object).
func parseReview(raw string) ([]rawFinding, error) {
	s := strings.TrimSpace(raw)
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
		return nil, errors.New("no JSON object in model reply")
	}
	var envelope struct {
		Findings []rawFinding `json:"findings"`
	}
	dec := json.NewDecoder(strings.NewReader(s[start:]))
	if err := dec.Decode(&envelope); err != nil { // one value; trailing junk ignored
		return nil, err
	}
	return envelope.Findings, nil
}

// locate resolves a model-quoted substring to an authoritative byte range in text.
// A repeated phrase is ambiguous, so it is only accepted when a key ties the finding to a
// known uncertain range — then the occurrence nearest that range wins. Without a key an
// ambiguous quote is dropped: silently highlighting the wrong occurrence is worse than
// showing one finding fewer.
func locate(text, from string, key string, byKey map[string]ReviewUncertain, wholeWords bool) (int, int, bool) {
	if from == "" {
		return 0, 0, false
	}
	var hits []int
	for off := 0; ; {
		i := strings.Index(text[off:], from)
		if i < 0 {
			break
		}
		at := off + i
		// A word-level replacement must not land inside a longer word: "να" occurs inside
		// "ναι", and rewriting it there would corrupt an unrelated word. Deletions of a
		// quoted phrase keep literal matching — the phrase carries its own boundaries.
		if !wholeWords || onWordBoundaries(text, at, at+len(from)) {
			hits = append(hits, at)
		}
		off = at + 1
	}
	switch {
	case len(hits) == 0:
		return 0, 0, false
	case len(hits) == 1:
		return hits[0], hits[0] + len(from), true
	}
	anchor, ok := byKey[key]
	if !ok {
		return 0, 0, false // ambiguous and unanchored
	}
	best, bestDist := hits[0], -1
	for _, h := range hits {
		d := h - anchor.Start
		if d < 0 {
			d = -d
		}
		if bestDist < 0 || d < bestDist {
			best, bestDist = h, d
		}
	}
	return best, best + len(from), true
}

// onWordBoundaries reports whether [start,end) is not glued to a letter or digit on
// either side. Combining marks count as part of the word, so a Greek accent is not a
// boundary.
func onWordBoundaries(text string, start, end int) bool {
	inWord := func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.In(r, unicode.M)
	}
	if start > 0 {
		r, _ := utf8.DecodeLastRuneInString(text[:start])
		if inWord(r) {
			return false
		}
	}
	if end < len(text) {
		r, _ := utf8.DecodeRuneInString(text[end:])
		if inWord(r) {
			return false
		}
	}
	return true
}

func trimRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max]))
}

// validateFindings turns untrusted model output into findings safe to show and to
// persist. Everything here exists because the alternative is a wrong highlight, a
// corrupted corrections.tsv line, or a silent no-op edit.
func validateFindings(text string, raws []rawFinding, unc []ReviewUncertain, mode ReviewMode) []Finding {
	byKey := make(map[string]ReviewUncertain, len(unc))
	for _, u := range unc {
		byKey[u.Key] = u
	}
	out := make([]Finding, 0, maxReviewFindings)
	seen := map[[2]int]bool{}
	for _, r := range raws {
		kind := strings.ToLower(strings.TrimSpace(r.Kind))
		switch kind {
		case KindMisheard, KindNonsense, KindFiller, KindRedundant:
		default:
			continue
		}
		if !mode.allowsKind(kind) {
			continue
		}
		from := r.From
		to := strings.TrimSpace(r.To)
		// A deletion must delete; a replacement must replace. A model that mixes them up
		// would otherwise produce a button that does the opposite of its label.
		if isDeleteKind(kind) {
			if to != "" {
				continue
			}
		} else if to == "" {
			continue
		}
		// `to` can end up in corrections.tsv / vocabulary.txt, both tab-and-newline
		// delimited, so control characters can never be allowed through.
		if hasControl(to) || strings.ContainsAny(to, "\t\n\r") {
			continue
		}
		if len([]rune(to)) > maxToRunes {
			continue
		}
		// `from` reaches corrections.tsv as a delete rule, so it is held to the same
		// delimiter rules as `to`.
		if strings.TrimSpace(from) == "" || hasControl(from) || strings.ContainsAny(from, "\t\n\r") {
			continue
		}
		// A whole-word check for single-word replacements only; a quoted phrase (which is
		// what deletions and multi-word fixes are) carries its own boundaries.
		wholeWords := !isDeleteKind(kind) && !strings.ContainsAny(from, " \t\n")
		start, end, ok := locate(text, from, r.Key, byKey, wholeWords)
		if !ok {
			continue
		}
		if !isDeleteKind(kind) && to == text[start:end] {
			continue // proposes no change
		}
		if seen[[2]int{start, end}] {
			continue
		}
		seen[[2]int{start, end}] = true
		out = append(out, Finding{
			Start: start, End: end, From: from, To: to, Kind: kind,
			Why: trimRunes(r.Why, maxWhyRunes),
		})
	}
	// Position order: there is no confidence signal in the schema, so any other ordering
	// would be invented. Overlaps are dropped rather than merged — applying one would
	// invalidate the other's range.
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	kept := out[:0]
	prevEnd := -1
	for _, f := range out {
		if f.Start < prevEnd {
			continue
		}
		kept = append(kept, f)
		prevEnd = f.End
		if len(kept) == maxReviewFindings {
			break
		}
	}
	return kept
}

// ClampReviewInput enforces the request limits in one place, so the handler and the tests
// agree on what "too big" means.
func ClampReviewInput(text string, unc []ReviewUncertain, vocab []string) (string, []ReviewUncertain, []string, error) {
	if len(text) > MaxReviewTextBytes {
		return "", nil, nil, fmt.Errorf("text too large (%d bytes, max %d)", len(text), MaxReviewTextBytes)
	}
	if len(unc) > MaxReviewUncertain {
		unc = unc[:MaxReviewUncertain]
	}
	clean := unc[:0:0]
	for _, u := range unc {
		if strings.TrimSpace(u.Word) == "" || u.Start < 0 || u.End > len(text) || u.Start >= u.End {
			continue
		}
		clean = append(clean, u)
	}
	if len(vocab) > MaxReviewVocab {
		vocab = vocab[:MaxReviewVocab]
	}
	terms := vocab[:0:0]
	for _, v := range vocab {
		t := strings.TrimSpace(v)
		if t != "" && !hasControl(t) {
			terms = append(terms, t)
		}
	}
	return text, clean, terms, nil
}

// Review asks the model to proof-read the transcript and returns validated findings.
// Every failure mode — no key, empty text, transport error, unparseable reply, deadline
// exhausted — yields an empty slice. The caller reports "no review", never an error to
// the user: a proof-reader that occasionally has no opinion is normal.
func Review(ctx context.Context, apiKey, text, lang string, unc []ReviewUncertain,
	vocab []string, models []string, caller modelFunc) ([]Finding, string, error) {
	return ReviewWithOptions(ctx, apiKey, text, lang, unc, vocab, models, caller,
		ReviewOptions{Mode: ReviewModeDefault})
}

// ReviewWithOptions is Review with the additive P16 knobs. Everything else —
// the six-second total deadline, the per-attempt budget, the fallback model
// chain, input limits, exact-substring location, non-overlap filtering and the
// six-finding cap — is unchanged.
func ReviewWithOptions(ctx context.Context, apiKey, text, lang string, unc []ReviewUncertain,
	vocab []string, models []string, caller modelFunc, opt ReviewOptions) ([]Finding, string, error) {

	if apiKey == "" || strings.TrimSpace(text) == "" {
		return nil, "", nil
	}
	if caller == nil {
		caller = realSuggestModel(apiKey) // JSON-mode transport, shared with Suggest
	}
	if len(models) == 0 {
		models = DefaultModels
	}
	prompt := buildReviewPrompt(text, lang, unc, vocab, opt.Mode)

	total, cancelTotal := context.WithTimeout(ctx, reviewTotalDeadline)
	defer cancelTotal()

	var lastErr error
	for _, model := range models {
		if total.Err() != nil {
			break
		}
		attempt, cancel := context.WithTimeout(total, reviewPerAttempt)
		raw, err := caller(attempt, model, prompt)
		cancel()
		if err != nil {
			lastErr = err
			continue // transport/quota failure → next bucket
		}
		raws, err := parseReview(raw)
		if err != nil {
			lastErr = fmt.Errorf("%w (raw: %.300q)", err, raw)
			continue
		}
		return validateFindings(text, raws, unc, opt.Mode), model, nil
	}
	return nil, "", lastErr
}
