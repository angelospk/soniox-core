package cleanup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseProfile(t *testing.T) {
	p := parseProfile("message", "# trigger: μήνυμα, message\nSummarize and fix.\nSecond line.")
	if p.Name != "message" {
		t.Errorf("name = %q", p.Name)
	}
	if strings.Join(p.Triggers, ",") != "μήνυμα,message" {
		t.Errorf("triggers = %v", p.Triggers)
	}
	if p.Body != "Summarize and fix.\nSecond line." {
		t.Errorf("body = %q", p.Body)
	}
}

func TestParseProfileNoTriggerLine(t *testing.T) {
	p := parseProfile("x", "just body\nhere")
	if len(p.Triggers) != 0 {
		t.Errorf("expected no triggers, got %v", p.Triggers)
	}
	if p.Body != "just body\nhere" {
		t.Errorf("body should be whole content, got %q", p.Body)
	}
}

func TestParseProfileSummarize(t *testing.T) {
	p := parseProfile("notes", "# trigger: notes\n# summarize: true\nOrganize the dump.")
	if !p.Summarize {
		t.Errorf("expected Summarize true")
	}
	if strings.Join(p.Triggers, ",") != "notes" {
		t.Errorf("triggers = %v", p.Triggers)
	}
	if p.Body != "Organize the dump." {
		t.Errorf("body = %q", p.Body)
	}
}

func TestParseProfileSummarizeOrderIndependent(t *testing.T) {
	// summarize line before trigger line
	p := parseProfile("x", "# summarize: true\n# trigger: a, b\nbody")
	if !p.Summarize || strings.Join(p.Triggers, ",") != "a,b" || p.Body != "body" {
		t.Errorf("got summarize=%v triggers=%v body=%q", p.Summarize, p.Triggers, p.Body)
	}
}

func TestParseProfileNoSummarizeByDefault(t *testing.T) {
	p := parseProfile("x", "# trigger: a\nbody")
	if p.Summarize {
		t.Errorf("expected Summarize false by default")
	}
}

func TestParseProfileCRLF(t *testing.T) {
	p := parseProfile("x", "# trigger: a\r\n# summarize: true\r\nbody")
	if !p.Summarize || strings.Join(p.Triggers, ",") != "a" || p.Body != "body" {
		t.Errorf("CRLF parse: summarize=%v triggers=%v body=%q", p.Summarize, p.Triggers, p.Body)
	}
}

func TestBuiltinProfilesSet(t *testing.T) {
	m := builtinMap()
	for _, name := range []string{"default", "message", "clean", "notes", "handoff", "summary"} {
		if _, ok := m[name]; !ok {
			t.Errorf("missing built-in %q", name)
		}
	}
	for _, name := range []string{"message", "notes", "handoff", "summary"} {
		if !m[name].Summarize {
			t.Errorf("%q should be Summarize", name)
		}
	}
	if m["default"].Summarize || m["clean"].Summarize {
		t.Errorf("default/clean must not summarize")
	}
}

func TestNewTriggersResolve(t *testing.T) {
	m := builtinMap()
	cases := map[string]string{
		"σημειώσεις": "notes", "notes": "notes", "οργάνωσε": "notes",
		"handoff": "handoff", "agent": "handoff", "εντολές": "handoff",
		"σύνοψη": "summary", "summary": "summary", "περίληψη": "summary",
	}
	for spoken, want := range cases {
		if got := resolveMode(spoken, m); got != want {
			t.Errorf("resolveMode(%q) = %q, want %q", spoken, got, want)
		}
	}
}

func TestSeedWritesSummarizeHeader(t *testing.T) {
	dir := t.TempDir()
	if err := seedProfiles(dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "notes.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "# summarize: true") {
		t.Errorf("notes.md must carry summarize header:\n%s", b)
	}
	p := parseProfile("notes", string(b))
	if !p.Summarize || len(p.Triggers) == 0 {
		t.Errorf("round-trip lost flag/triggers: %+v", p)
	}
}

func TestFoldGreek(t *testing.T) {
	cases := map[string]string{
		"ΜΉΝΥΜΑ":  "μηνυμα",
		"εντολή":  "εντολη",
		"Εντολή":  "εντολη",
		"ΐϋΰ":     "ιυυ",
		"Message": "message",
	}
	for in, want := range cases {
		if got := foldGreek(in); got != want {
			t.Errorf("foldGreek(%q) = %q, want %q", in, got, want)
		}
	}
}

func sampleProfiles() map[string]Profile {
	return map[string]Profile{
		"default": {Name: "default"},
		"message": {Name: "message", Triggers: []string{"μήνυμα", "message"}},
		"clean":   {Name: "clean", Triggers: []string{"καθαρό"}},
	}
}

func TestParseCommandProfileSwitch(t *testing.T) {
	profile, adHoc, strip, ok := ParseCommand("γεια σου ραντεβού αύριο, εντολή μήνυμα.", "εντολή", sampleProfiles())
	if !ok || profile != "message" || adHoc != "" {
		t.Fatalf("ok=%v profile=%q adHoc=%q", ok, profile, adHoc)
	}
	if strip != "γεια σου ραντεβού αύριο" {
		t.Errorf("strip = %q", strip)
	}
}

func TestParseCommandProfileNoCommaStillWorks(t *testing.T) {
	// Profile branch needs no boundary — the daily "εντολή μήνυμα" keeps working.
	profile, adHoc, _, ok := ParseCommand("τελείωσα εντολή μήνυμα", "εντολή", sampleProfiles())
	if !ok || profile != "message" || adHoc != "" {
		t.Fatalf("ok=%v profile=%q adHoc=%q", ok, profile, adHoc)
	}
}

func TestParseCommandCaseAccentInsensitive(t *testing.T) {
	profile, _, _, ok := ParseCommand("κάτι κάτι ΕΝΤΟΛΗ Μήνυμα", "εντολή", sampleProfiles())
	if !ok || profile != "message" {
		t.Fatalf("ok=%v profile=%q", ok, profile)
	}
}

func TestParseCommandNameAsTrigger(t *testing.T) {
	profile, _, _, ok := ParseCommand("blah blah εντολή clean", "εντολή", sampleProfiles())
	if !ok || profile != "clean" {
		t.Fatalf("ok=%v profile=%q", ok, profile)
	}
}

func TestParseCommandFreeFormWithComma(t *testing.T) {
	profile, adHoc, strip, ok := ParseCommand("να βρεθούμε αύριο, εντολή κάνε το πιο επίσημο.", "εντολή", sampleProfiles())
	if !ok || profile != "" || adHoc != "κάνε το πιο επίσημο" {
		t.Fatalf("ok=%v profile=%q adHoc=%q", ok, profile, adHoc)
	}
	if strip != "να βρεθούμε αύριο" {
		t.Errorf("strip = %q", strip)
	}
}

func TestParseCommandSwitchPlusAdHoc(t *testing.T) {
	profile, adHoc, _, ok := ParseCommand("το κείμενο, εντολή μήνυμα κάνε το σύντομο", "εντολή", sampleProfiles())
	if !ok || profile != "message" || adHoc != "κάνε το σύντομο" {
		t.Fatalf("ok=%v profile=%q adHoc=%q", ok, profile, adHoc)
	}
}

func TestParseCommandFreeFormNoBoundaryIgnored(t *testing.T) {
	in := "σου έδωσα εντολή να φύγεις"
	profile, adHoc, strip, ok := ParseCommand(in, "εντολή", sampleProfiles())
	if ok || profile != "" || adHoc != "" || strip != in {
		t.Errorf("mid-clause free-form must not trigger: ok=%v profile=%q adHoc=%q strip=%q", ok, profile, adHoc, strip)
	}
}

func TestParseCommandUnknownModeNoCommaIgnored(t *testing.T) {
	in := "κάτι εντολή άγνωστο"
	_, _, strip, ok := ParseCommand(in, "εντολή", sampleProfiles())
	if ok || strip != in {
		t.Errorf("unknown mode w/o boundary must not trigger; ok=%v strip=%q", ok, strip)
	}
}

func TestParseCommandKeywordOnlyNoTail(t *testing.T) {
	_, _, _, ok := ParseCommand("τελείωσα εντολή", "εντολή", sampleProfiles())
	if ok {
		t.Errorf("keyword with no tail should not trigger")
	}
}

func TestParseCommandSemicolonBoundary(t *testing.T) {
	// A semicolon (own token, space before the keyword) is a valid pause boundary.
	profile, adHoc, _, ok := ParseCommand("κάτι; εντολή κάνε το λίστα", "εντολή", sampleProfiles())
	if !ok || profile != "" || adHoc != "κάνε το λίστα" {
		t.Fatalf("ok=%v profile=%q adHoc=%q", ok, profile, adHoc)
	}
}

func TestParseCommandLeadingPunctGluedToKeyword(t *testing.T) {
	// Punctuation glued to the keyword's front (";εντολή") is itself the boundary.
	profile, adHoc, _, ok := ParseCommand("κάτι ;εντολή κάνε το επίσημο", "εντολή", sampleProfiles())
	if !ok || profile != "" || adHoc != "κάνε το επίσημο" {
		t.Fatalf("ok=%v profile=%q adHoc=%q", ok, profile, adHoc)
	}
}

func TestParseCommandKeywordFirstToken(t *testing.T) {
	// Keyword as the very first token satisfies the boundary.
	profile, adHoc, strip, ok := ParseCommand("εντολή κάνε το λίστα", "εντολή", sampleProfiles())
	if !ok || profile != "" || adHoc != "κάνε το λίστα" || strip != "" {
		t.Fatalf("ok=%v profile=%q adHoc=%q strip=%q", ok, profile, adHoc, strip)
	}
}

func TestLoadProfilesSeedsAndReads(t *testing.T) {
	dir := t.TempDir()
	// empty dir -> seeds built-ins
	p, err := LoadProfiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"default", "message", "clean"} {
		if _, ok := p[want]; !ok {
			t.Errorf("built-in profile %q missing after seed", want)
		}
		if _, err := os.Stat(filepath.Join(dir, want+".md")); err != nil {
			t.Errorf("seed file for %q not written: %v", want, err)
		}
	}
	// user edit is honored on next load
	if err := os.WriteFile(filepath.Join(dir, "message.md"), []byte("# trigger: msg\nCUSTOM BODY"), 0o644); err != nil {
		t.Fatalf("write edited profile: %v", err)
	}
	p2, err := LoadProfiles(dir)
	if err != nil {
		t.Fatalf("reload profiles: %v", err)
	}
	if p2["message"].Body != "CUSTOM BODY" {
		t.Errorf("edited body not read back, got %q", p2["message"].Body)
	}
	if strings.Join(p2["message"].Triggers, ",") != "msg" {
		t.Errorf("edited triggers not read back, got %v", p2["message"].Triggers)
	}
}

func TestProfileBodyFallback(t *testing.T) {
	profs := map[string]Profile{"default": {Name: "default", Body: "X"}}
	if ProfileBody(profs, "nope") != instructions {
		t.Errorf("unknown profile should fall back to built-in instructions")
	}
	if ProfileBody(profs, "default") != "X" {
		t.Errorf("known profile body not returned")
	}
	if ProfileBody(map[string]Profile{"default": {Name: "default", Body: ""}}, "default") != instructions {
		t.Errorf("empty body should fall back to built-in instructions")
	}
}
