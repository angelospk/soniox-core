package cleanup

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DefaultKeyword is the spoken marker word that precedes a mode word in a voice
// command, e.g. "…<text>, εντολή μήνυμα". Configurable per deployment, but the
// matching logic itself is fixed code so a bad config can never break dictation.
const DefaultKeyword = "εντολή"

// Profile is one named cleanup prompt. Only Body is user-editable; Triggers and
// the selection logic are fixed code.
type Profile struct {
	Name      string
	Triggers  []string // spoken mode words that select this profile
	Body      string   // instructions sent to Gemini (replaces the default block)
	Summarize bool     // condensing profile — output may be much shorter than input
}

// builtinProfiles seed the prompts dir on first run and act as the fallback if a
// file is missing or empty. "default" reuses the conservative instructions const.
var builtinProfiles = []Profile{
	{Name: "default", Triggers: []string{"απλό", "default"}, Body: instructions},
	{Name: "clean", Triggers: []string{"καθαρό", "clean"}, Body: cleanBody},
	{Name: "message", Triggers: []string{"μήνυμα", "message"}, Body: messageBody, Summarize: true},
	{Name: "notes", Triggers: []string{"σημειώσεις", "notes", "οργάνωσε"}, Body: notesBody, Summarize: true},
	{Name: "handoff", Triggers: []string{"agent", "handoff", "εντολές"}, Body: handoffBody, Summarize: true},
	{Name: "summary", Triggers: []string{"σύνοψη", "summary", "περίληψη"}, Body: summaryBody, Summarize: true},
}

const messageBody = `You turn a raw speech-to-text transcript into a clean message ready to send (chat/SMS/DM).

KEEP every piece of information and intent: each point, request, question, name, number and decision. Do NOT drop a detail just to be shorter — completeness comes first.
CONDENSE by removing only filler, hesitations, false starts, stutters and verbatim repetition. If the speaker made the same point twice in different words, merge it into one clear statement — never into a vague generality.
LANGUAGE: preserve the original language(s). Greek and English may mix — keep each word in the language it was spoken. NEVER translate.
TONE: keep the speaker's own voice — clearer, not more formal or AI-sounding. No greeting and no sign-off unless the speaker clearly dictated one; do not invent a recipient or salutation.
FIX spelling, capitalization and punctuation.
VOCABULARY: use the list ONLY to correct clear mis-hearings of listed terms; never insert a term that was not said; never mention the vocabulary.

Output ONLY the message text. No preamble, explanations, quotes, markdown fences or em dashes.`

const cleanBody = `You fix a raw speech-to-text transcript with a light, conservative touch.

RULES:
- Preserve the speaker's exact words, vocabulary, tone, meaning and language(s). Greek and English may mix — keep each word in its spoken language. NEVER translate, rephrase, summarize or add anything.
- Fix ONLY clear grammar/syntax mistakes, obvious mis-transcriptions, spelling, capitalization, punctuation, and phrases that make no sense as transcribed.
- When in doubt, leave it exactly as the speaker said it. Prefer under-correcting over changing meaning.
- VOCABULARY: use the list ONLY to correct a word clearly mis-heard as a near-match of a listed term. Never insert a listed term that was not said. Never mention or explain the vocabulary.
- Output ONLY the corrected transcript. No preamble, no explanations, no quotes, no markdown fences, no em dashes.`

const notesBody = `You turn a raw spoken brain-dump into organized notes.

KEEP every distinct point, idea, fact, name, number, decision and open question — completeness matters more than brevity. Better a slightly longer note than a lost detail.
ORGANIZE into a few clear headed sections or bullet points; keep it scannable.
CONDENSE by merging duplicated or repeated points into one and dropping filler, hesitations and false starts. Never collapse distinct points into a single vague summary line.
Do NOT invent, expand or editorialize.
LANGUAGE: preserve the original language(s); Greek and English may mix — keep each word as spoken. NEVER translate.
FIX spelling, capitalization and punctuation.
VOCABULARY: use the list ONLY to correct clear mis-hearings of listed terms; never insert a term that was not said; never mention the vocabulary.

Output ONLY the organized notes. No preamble, explanations, quotes or markdown fences.`

const handoffBody = `You turn rambling spoken notes into clear, structured instructions for a coding agent.

Produce: a one-line Goal, then concrete Steps/Tasks (numbered or bulleted), then Constraints, then any Context the agent needs. Omit a section ONLY if the speaker gave nothing for it.
KEEP every actual requirement, decision, file/name, number and constraint the speaker stated — do NOT drop details to be concise. If the speaker was unsure or flagged something as optional, keep it and mark it as such.
Be precise and imperative. Drop filler, hesitations, false starts and repetition.
Do NOT invent requirements, file names or APIs that were not mentioned.
Keep technical and proper terms (code, commands, library/API/file names) exactly as written in their usual form.
FIX spelling, capitalization and punctuation.
VOCABULARY: use the list ONLY to correct clear mis-hearings of listed terms; never insert a term that was not said; never mention the vocabulary.

Output ONLY the instructions. No preamble, explanations, quotes or markdown fences.`

const summaryBody = `You compress a raw speech-to-text transcript into a tight but complete summary.

KEEP every distinct key point, decision, name and number. Be compact, but do NOT omit a substantive point — losing meaning is worse than being a little longer.
DROP filler, hesitations, false starts, repetition and asides. Use short sentences or a few bullets.
LANGUAGE: keep the original language; NEVER translate. Keep technical and proper terms as written.
Do NOT invent or editorialize.
VOCABULARY: use the list ONLY to correct clear mis-hearings of listed terms; never insert a term that was not said; never mention the vocabulary.

Output ONLY the summary. No preamble, explanations, quotes or markdown fences.`

// greekFold maps lowercase accented Greek (and final sigma) to plain forms so
// trigger matching is accent-insensitive without an external dependency.
var greekFold = strings.NewReplacer(
	"ά", "α", "έ", "ε", "ή", "η", "ί", "ι", "ό", "ο", "ύ", "υ", "ώ", "ω",
	"ϊ", "ι", "ϋ", "υ", "ΐ", "ι", "ΰ", "υ", "ς", "σ",
)

// foldGreek lowercases and strips Greek diacritics for case/accent-insensitive
// comparison of single words. Combining marks are dropped too, so decomposed (NFD)
// accents fold the same as precomposed ones.
func foldGreek(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Mn, r) {
			return -1
		}
		return r
	}, s)
	return greekFold.Replace(strings.ToLower(s))
}

// trailCut is trimmed from the end of an utterance (and tokens) before matching.
const trailCut = " \t\r\n.,!;:·…?·"

// parseProfile reads a profile file: optional leading header lines
// "# trigger: a, b" and "# summarize: true" (any order, case-insensitive),
// followed by the prompt body. A missing trigger line yields no triggers and
// the whole content as the body.
func parseProfile(name, content string) Profile {
	p := Profile{Name: name}
	rest := content
	for {
		line := rest
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			line = rest[:i]
		}
		trimmed := strings.TrimSpace(line)
		low := strings.ToLower(trimmed)
		switch {
		case strings.HasPrefix(low, "# trigger:"):
			raw := trimmed[strings.IndexByte(trimmed, ':')+1:]
			for _, t := range strings.Split(raw, ",") {
				if t = strings.TrimSpace(t); t != "" {
					p.Triggers = append(p.Triggers, t)
				}
			}
		case strings.HasPrefix(low, "# summarize:"):
			val := strings.TrimSpace(trimmed[strings.IndexByte(trimmed, ':')+1:])
			p.Summarize = strings.EqualFold(val, "true")
		default:
			p.Body = strings.TrimSpace(rest)
			return p
		}
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			rest = rest[i+1:]
		} else {
			rest = ""
		}
	}
}

// LoadProfiles seeds the dir with the built-ins on first use, then reads every
// *.md profile. Built-ins are always present in the result even if their file
// was deleted, so selection can never fail to resolve a known profile.
func LoadProfiles(dir string) (map[string]Profile, error) {
	out := builtinMap()
	if err := seedProfiles(dir); err != nil {
		return out, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		b, err := os.ReadFile(filepath.Join(dir, e.Name())) // #nosec G304 -- fixed prompts dir
		if err != nil {
			continue
		}
		out[name] = parseProfile(name, string(b))
	}
	return out, nil
}

func builtinMap() map[string]Profile {
	m := make(map[string]Profile, len(builtinProfiles))
	for _, p := range builtinProfiles {
		m[p.Name] = p
	}
	return m
}

// seedProfiles writes any missing built-in file without overwriting user edits.
func seedProfiles(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	for _, p := range builtinProfiles {
		fp := filepath.Join(dir, p.Name+".md")
		if _, err := os.Stat(fp); err == nil {
			continue
		}
		content := "# trigger: " + strings.Join(p.Triggers, ", ") + "\n"
		if p.Summarize {
			content += "# summarize: true\n"
		}
		content += p.Body + "\n"
		if err := os.WriteFile(fp, []byte(content), 0o644); err != nil { // #nosec G306 -- non-secret prompt text
			return err
		}
	}
	return nil
}

// ProfileBody returns the named profile's body, falling back to the built-in
// default instructions if the profile is unknown or its body is empty.
func ProfileBody(profiles map[string]Profile, name string) string {
	if p, ok := profiles[name]; ok && strings.TrimSpace(p.Body) != "" {
		return p.Body
	}
	return instructions
}

// boundaryMarks are the punctuation that mark a prosodic pause before a free-form
// command keyword. A free-form "εντολή" counts only after one of these (or at the
// very start) — so ordinary mid-clause speech ("έδωσα εντολή να…") never triggers.
// Includes ASCII ";"/"?", the Greek ano teleia (U+0387) and middot (U+00B7), and
// the Greek question mark (U+037E).
const boundaryMarks = ",.!;··…?;;"

// ParseCommand detects a trailing voice command introduced by the last `keyword`
// ("εντολή") token. It supports two forms, in priority order:
//
//  1. profile switch: the first token after the keyword resolves to a known profile.
//     Any remaining tokens become a free-form instruction (switch + augment). This
//     form needs NO boundary before the keyword (the known profile is the safeguard).
//  2. free-form: the keyword sits at a prosodic boundary (preceded by punctuation in
//     boundaryMarks, or is the first token); everything after it is the instruction.
//
// Returns the selected profile ("" = keep the active profile), the ad-hoc instruction
// ("" = none), the transcript with the command region stripped, and whether a command
// was found. On no match it returns ("", "", raw, false).
func ParseCommand(raw, keyword string, profiles map[string]Profile) (profile, adHoc, stripped string, found bool) {
	if strings.TrimSpace(keyword) == "" {
		keyword = DefaultKeyword
	}
	foldKw := foldGreek(keyword)

	// Tokenize with byte offsets so we can inspect the char before the keyword.
	type tok struct {
		text       string
		start, end int
	}
	var toks []tok
	inTok, s := false, 0
	for i, r := range raw {
		sep := unicode.IsSpace(r) // incl. no-break space, which Soniox may emit
		if sep {
			if inTok {
				toks = append(toks, tok{raw[s:i], s, i})
				inTok = false
			}
		} else if !inTok {
			s, inTok = i, true
		}
	}
	if inTok {
		toks = append(toks, tok{raw[s:], s, len(raw)})
	}

	// Last token equal to the keyword (accent/case-insensitive, surrounding punct ok).
	kwIdx := -1
	for i := len(toks) - 1; i >= 0; i-- {
		if foldGreek(strings.Trim(toks[i].text, trailCut)) == foldKw {
			kwIdx = i
			break
		}
	}
	if kwIdx < 0 || kwIdx == len(toks)-1 {
		return "", "", raw, false // no keyword, or nothing follows it
	}

	// The keyword token may carry leading punctuation (e.g. "·εντολή"); the keyword's
	// real start is after it, and that stripped punctuation is itself a valid pause
	// boundary — so use the adjusted offset for both the boundary check and the strip.
	lead := len(toks[kwIdx].text) - len(strings.TrimLeft(toks[kwIdx].text, trailCut))
	kwStart := toks[kwIdx].start + lead

	content := strings.TrimRight(raw[:kwStart], trailCut)
	firstTail := strings.Trim(toks[kwIdx+1].text, trailCut)
	tailAll := strings.Trim(strings.TrimSpace(raw[toks[kwIdx].end:]), trailCut)

	// (1) profile switch — no boundary needed.
	if pname := resolveMode(firstTail, profiles); pname != "" {
		rest := strings.Trim(strings.TrimSpace(raw[toks[kwIdx+1].end:]), trailCut)
		return pname, rest, content, true
	}

	// (2) free-form — require a boundary before the keyword.
	if !boundaryBefore(raw, kwStart) || tailAll == "" {
		return "", "", raw, false
	}
	return "", tailAll, content, true
}

// boundaryBefore reports whether the keyword starting at byte index `start` is preceded
// by a prosodic boundary: only whitespace back to the string start, or a punctuation
// rune in boundaryMarks immediately before (ignoring whitespace).
func boundaryBefore(raw string, start int) bool {
	pre := strings.TrimRight(raw[:start], " \t\r\n")
	if pre == "" {
		return true // keyword is the first token
	}
	r, _ := utf8.DecodeLastRuneInString(pre)
	return strings.ContainsRune(boundaryMarks, r)
}

// resolveMode maps a spoken mode word to a profile name via the profile's name
// or its trigger list (accent/case-insensitive). Returns "" if unrecognized.
func resolveMode(mode string, profiles map[string]Profile) string {
	f := foldGreek(strings.Trim(mode, trailCut))
	if f == "" {
		return ""
	}
	for _, p := range profiles {
		if foldGreek(p.Name) == f {
			return p.Name
		}
		for _, t := range p.Triggers {
			if foldGreek(t) == f {
				return p.Name
			}
		}
	}
	return ""
}

// SendWord, spoken as the LAST word of a voice command ("…, εντολή στείλε"), asks the
// client to press Return after pasting, so a chat message is sent hands-free. It can
// follow a profile or ad-hoc command ("εντολή μήνυμα στείλε"). A bare "<keyword>
// στείλε" needs no pause before the keyword: the two-word phrase is the safeguard.
const SendWord = "στείλε"

// ParseCommandSend is ParseCommand plus the trailing SendWord. send reports whether
// the command ended in SendWord; the word is stripped along with the command. A
// trailing "στείλε" with no command keyword before it is ordinary speech.
func ParseCommandSend(raw, keyword string, profiles map[string]Profile) (profile, adHoc, stripped string, found, send bool) {
	if strings.TrimSpace(keyword) == "" {
		keyword = DefaultKeyword
	}
	if rest, ok := cutLastWord(raw, SendWord); ok {
		if pre, ok := cutLastWord(rest, keyword); ok {
			return "", "", strings.TrimRightFunc(pre, isCut), true, true
		}
		if p, a, s, ok := ParseCommand(rest, keyword, profiles); ok {
			return p, a, s, true, true
		}
	}
	profile, adHoc, stripped, found = ParseCommand(raw, keyword, profiles)
	return profile, adHoc, stripped, found, false
}

// cutLastWord reports whether the last whitespace-separated token of s equals word
// (accent/case-insensitive, surrounding punctuation ignored) and returns s before it.
// Any Unicode space separates tokens (Soniox may emit a no-break space).
func cutLastWord(s, word string) (string, bool) {
	t := strings.TrimRightFunc(s, isCut)
	i := 0 // start of the last token: just past the last space rune (1-3 bytes)
	if j := strings.LastIndexFunc(t, unicode.IsSpace); j >= 0 {
		_, n := utf8.DecodeRuneInString(t[j:])
		i = j + n
	}
	if t[i:] == "" || foldGreek(strings.TrimFunc(t[i:], isCut)) != foldGreek(word) {
		return s, false
	}
	return t[:i], true
}

// isCut is trailCut plus any Unicode space and the Greek question mark (U+037E).
func isCut(r rune) bool {
	return unicode.IsSpace(r) || r == '\u037e' || strings.ContainsRune(trailCut, r)
}
