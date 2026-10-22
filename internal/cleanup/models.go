package cleanup

import (
	"os"
	"strings"
)

// ReadModelChain parses a model fallback chain file: one model per line, or comma/
// space/tab separated; '#' starts a comment. A missing or empty file returns nil,
// which callers treat as DefaultModels.
func ReadModelChain(path string) []string {
	b, err := os.ReadFile(path) // #nosec G304 -- fixed prefs path chosen by the caller
	if err != nil {
		return nil
	}
	var models []string
	for _, ln := range strings.Split(string(b), "\n") {
		if i := strings.IndexByte(ln, '#'); i >= 0 {
			ln = ln[:i]
		}
		for _, tok := range strings.FieldsFunc(ln, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\r' }) {
			models = append(models, tok)
		}
	}
	return models
}
