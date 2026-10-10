// Package soniox wraps github.com/moxierobots/soniox-stt-go (the real-time
// Soniox STT WebSocket SDK) and assembles its partial/final token stream into
// text. We don't reimplement the protocol — only the text assembly + a small,
// channel-friendly streaming helper.
package soniox

import (
	"math"
	"strings"
	"unicode"
	"unicode/utf16"
)

// Audio constants for the PCM we feed Soniox.
const (
	SampleRate  = 24000
	NumChannels = 1
	AudioFormat = "pcm_s16le"
)

// ChunkBytes is the byte size of a ms-millisecond PCM s16le mono chunk.
func ChunkBytes(ms int) int { return SampleRate * 2 * ms / 1000 }

// TokenLite is the minimal token info the assembler needs.
type TokenLite struct {
	Text       string
	IsFinal    bool
	Confidence float64
}

// LowConfidenceWords groups the final tokens into whole words (Soniox tokens can be
// sub-word and use whitespace to mark boundaries) and returns the lexical words whose
// minimum confidence — over their letter/number runes only, so attached punctuation
// can't drag a word down — is below threshold. Words are de-duplicated; punctuation
// is trimmed so they match the client's word-level lookup.
func LowConfidenceWords(tokens []TokenLite, threshold float64) []string {
	type rc struct {
		r rune
		c float64
	}
	var chars []rc
	for _, tk := range tokens {
		if !tk.IsFinal {
			continue
		}
		for _, r := range tk.Text {
			chars = append(chars, rc{r, tk.Confidence})
		}
	}
	var out []string
	seen := map[string]bool{}
	i := 0
	for i < len(chars) {
		if unicode.IsSpace(chars[i].r) {
			i++
			continue
		}
		j := i
		minConf := math.MaxFloat64
		hasLex := false
		for j < len(chars) && !unicode.IsSpace(chars[j].r) {
			if unicode.IsLetter(chars[j].r) || unicode.IsNumber(chars[j].r) {
				hasLex = true
				if chars[j].c < minConf {
					minConf = chars[j].c
				}
			}
			j++
		}
		if hasLex && minConf < threshold {
			runes := make([]rune, 0, j-i)
			for k := i; k < j; k++ {
				runes = append(runes, chars[k].r)
			}
			word := strings.TrimFunc(string(runes), func(r rune) bool {
				return !unicode.IsLetter(r) && !unicode.IsNumber(r)
			})
			key := strings.ToLower(word)
			if word != "" && !seen[key] {
				seen[key] = true
				out = append(out, word)
			}
		}
		i = j
	}
	return out
}

// LowConfRange is a character range (UTF-16 code units, to match the macOS client's
// NSRange) into the assembled final text of one OnFinal message.
type LowConfRange struct {
	Start int `json:"start"`
	Len   int `json:"len"`
}

// LowConfidenceRanges returns one range per low-confidence lexical word, positioned in
// the final text built by concatenating the final tokens' Text (exactly what OnFinal
// receives — Assembler.AddResult writes the same final tokens to nf, so these offsets
// are relative to nf). Unlike LowConfidenceWords it does NOT de-duplicate, so each
// uncertain occurrence is highlighted on its own. Punctuation attached to a word is
// trimmed from the range, mirroring the word-trimming in LowConfidenceWords.
func LowConfidenceRanges(tokens []TokenLite, threshold float64) []LowConfRange {
	// Flatten final tokens to runes, tracking each rune's confidence, its UTF-16 start
	// offset, and its UTF-16 length, so word end = lastRune.u16 + lastRune.u16Len.
	type rc struct {
		r      rune
		c      float64
		u16    int
		u16Len int
	}
	var chars []rc
	u16 := 0
	for _, tk := range tokens {
		if !tk.IsFinal {
			continue
		}
		for _, r := range tk.Text {
			n := len(utf16.Encode([]rune{r}))
			chars = append(chars, rc{r, tk.Confidence, u16, n})
			u16 += n
		}
	}
	var out []LowConfRange
	i := 0
	for i < len(chars) {
		if unicode.IsSpace(chars[i].r) {
			i++
			continue
		}
		j := i
		minConf := math.MaxFloat64
		hasLex := false
		for j < len(chars) && !unicode.IsSpace(chars[j].r) {
			if unicode.IsLetter(chars[j].r) || unicode.IsNumber(chars[j].r) {
				hasLex = true
				if chars[j].c < minConf {
					minConf = chars[j].c
				}
			}
			j++
		}
		if hasLex && minConf < threshold {
			ls := i
			for ls < j && !unicode.IsLetter(chars[ls].r) && !unicode.IsNumber(chars[ls].r) {
				ls++
			}
			le := j - 1
			for le > ls && !unicode.IsLetter(chars[le].r) && !unicode.IsNumber(chars[le].r) {
				le--
			}
			start := chars[ls].u16
			end := chars[le].u16 + chars[le].u16Len
			out = append(out, LowConfRange{Start: start, Len: end - start})
		}
		i = j
	}
	return out
}

// Assembler turns the stream of result messages into final + partial text:
// final tokens are appended once; non-final tokens are the live tail and are
// replaced on every message.
type Assembler struct {
	final   strings.Builder
	partial string
}

// AddResult processes one result message's tokens and returns the text newly
// finalized by this message (may be empty).
func (a *Assembler) AddResult(tokens []TokenLite) (newFinal string) {
	var nf strings.Builder
	partial := ""
	for _, tk := range tokens {
		if isControlToken(tk.Text) {
			continue
		}
		if tk.IsFinal {
			a.final.WriteString(tk.Text)
			nf.WriteString(tk.Text)
		} else {
			partial += tk.Text
		}
	}
	a.partial = partial
	return nf.String()
}

// isControlToken reports Soniox boundary markers: "<fin>" (manual finalization) and
// "<end>" (endpoint detection) arrive as final tokens but are not speech.
func isControlToken(t string) bool { return t == "<fin>" || t == "<end>" }

// Final returns the finalized transcript so far.
func (a *Assembler) Final() string { return a.final.String() }

// Partial returns the current not-yet-final tail.
func (a *Assembler) Partial() string { return a.partial }

// Full returns final + partial, trimmed.
func (a *Assembler) Full() string {
	return strings.TrimSpace(a.final.String() + a.partial)
}
