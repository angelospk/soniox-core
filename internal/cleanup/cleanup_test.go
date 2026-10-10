package cleanup

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBuildPromptVerbatimAndBraces(t *testing.T) {
	text := "say {hello} and } weird { braces"
	p := BuildPrompt(text, "OpenCouncil")
	if !strings.Contains(p, text) {
		t.Error("transcript not included verbatim")
	}
	if !strings.Contains(p, "OpenCouncil") {
		t.Error("vocab not included")
	}
}

func TestBuildPromptEmptyVocab(t *testing.T) {
	if !strings.Contains(BuildPrompt("hi", ""), "(none)") {
		t.Error("empty vocab should render (none)")
	}
}

func TestLangDirectiveGreek(t *testing.T) {
	out := buildPromptBody("BODY", "κείμενο", "(none)", "el", "")
	if !strings.Contains(out, "in Greek") {
		t.Errorf("expected Greek directive, got:\n%s", out)
	}
	if !strings.Contains(out, "technical") {
		t.Errorf("expected technical-terms carve-out, got:\n%s", out)
	}
	if strings.Index(out, "in Greek") > strings.Index(out, "BODY") {
		t.Errorf("directive must precede body")
	}
}

func TestLangDirectiveAutoEmpty(t *testing.T) {
	out := buildPromptBody("BODY", "text", "(none)", "auto", "")
	if strings.Contains(out, "Write the entire output") {
		t.Errorf("auto must inject no directive, got:\n%s", out)
	}
	if buildPromptBody("BODY", "text", "(none)", "", "") != out {
		t.Errorf("empty lang must equal auto")
	}
}

func TestLangDirective(t *testing.T) {
	if langDirective("el") == "" || langDirective("en") == "" {
		t.Errorf("el/en must produce a directive")
	}
	if langDirective("auto") != "" || langDirective("") != "" || langDirective("xx") != "" {
		t.Errorf("auto/empty/unknown must produce no directive")
	}
}

// accept is the keep-raw-vs-use-cleaned decision — the core AllowShrink seam.
func TestAcceptShrink(t *testing.T) {
	long := strings.Repeat("word ", 100)
	short := "tiny"
	if accept("", long, true) {
		t.Errorf("empty output must always be rejected")
	}
	if accept(short, long, false) {
		t.Errorf("big shrink must be rejected when AllowShrink=false")
	}
	if !accept(short, long, true) {
		t.Errorf("big shrink must be accepted when AllowShrink=true")
	}
	if !accept(long, long, false) {
		t.Errorf("same-length output must be accepted")
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"  hello  ":            "hello",
		"```\nhello\n```":      "hello",
		"```text\nhi there```": "hi there",
		"plain":                "plain",
	}
	for in, want := range cases {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTooShort(t *testing.T) {
	in := strings.Repeat("α", 100)
	if !tooShort(strings.Repeat("α", 50), in) {
		t.Error("50/100 should be too short")
	}
	if tooShort(strings.Repeat("α", 95), in) {
		t.Error("95/100 should be fine")
	}
}

func TestCleanEmptyInput(t *testing.T) {
	out, err := Clean(context.Background(), "key", "   ", Options{})
	if err != nil || out != "   " {
		t.Errorf("empty input should pass through, got %q err %v", out, err)
	}
}

func TestCleanNoKey(t *testing.T) {
	out, err := Clean(context.Background(), "", "hello", Options{})
	if err != nil || out != "hello" {
		t.Errorf("no key should pass through raw, got %q err %v", out, err)
	}
}

// fakeCaller builds a modelFunc returning the given output/error, recording calls.
func fakeCaller(out string, err error, calls *int) modelFunc {
	return func(ctx context.Context, _, _ string) (string, error) {
		if calls != nil {
			*calls++
		}
		return out, err
	}
}

func TestExtractAnswerSkipsThought(t *testing.T) {
	var gr geminiResp
	gr.Candidates = make([]struct {
		Content content `json:"content"`
	}, 1)
	gr.Candidates[0].Content.Parts = []part{
		{Text: "internal reasoning…", Thought: true},
		{Text: "the actual answer"},
	}
	if got := extractAnswer(gr); got != "the actual answer" {
		t.Errorf("thought part must be excluded, got %q", got)
	}
}

func TestGenConfigPerModel(t *testing.T) {
	if _, ok := genConfig("gemma-4-31b-it")["thinkingConfig"]; ok {
		t.Error("Gemma must NOT receive thinkingConfig (it returns 400)")
	}
	if _, ok := genConfig("gemini-2.5-flash-lite")["thinkingConfig"]; !ok {
		t.Error("Gemini should receive thinkingConfig")
	}
}

func TestModelChain(t *testing.T) {
	if got := (Options{}).modelChain(); len(got) != len(DefaultModels) {
		t.Errorf("empty options must use DefaultModels, got %v", got)
	}
	if got := (Options{Model: "x"}).modelChain(); len(got) != 1 || got[0] != "x" {
		t.Errorf("single Model must yield [x], got %v", got)
	}
	if got := (Options{Models: []string{"a", "b"}}).modelChain(); len(got) != 2 || got[1] != "b" {
		t.Errorf("explicit Models must win, got %v", got)
	}
}

func TestChainFallsThrough(t *testing.T) {
	in := strings.Repeat("λέξη ", 30)
	var used []string
	caller := func(ctx context.Context, model, _ string) (string, error) {
		used = append(used, model)
		if model == "bad" {
			return "", errors.New("gemini HTTP 429: slow down")
		}
		return "καθαρισμένο " + in, nil
	}
	r := CleanResult(context.Background(), "key", in, Options{Models: []string{"bad", "good"}, caller: caller})
	if r.Outcome != Cleaned {
		t.Fatalf("want Cleaned after fallthrough, got %s", r.Outcome)
	}
	if r.Model != "good" {
		t.Errorf("want Model=good, got %q", r.Model)
	}
	if len(used) != 2 || used[0] != "bad" || used[1] != "good" {
		t.Errorf("want chain [bad good], got %v", used)
	}
}

func TestOutcomeCleaned(t *testing.T) {
	in := strings.Repeat("λέξη ", 30)
	var calls int
	r := CleanResult(context.Background(), "key", in, Options{caller: fakeCaller("καθαρισμένο "+in, nil, &calls)})
	if r.Outcome != Cleaned {
		t.Fatalf("want Cleaned, got %s (reason %s)", r.Outcome, r.Reason)
	}
	if calls != 1 {
		t.Errorf("want 1 model call, got %d", calls)
	}
}

func TestOutcomeFallbackError(t *testing.T) {
	in := strings.Repeat("λέξη ", 30)
	var calls int
	r := CleanResult(context.Background(), "key", in,
		Options{Models: []string{"m1", "m2", "m3"}, caller: fakeCaller("", errors.New("gemini HTTP 503: down"), &calls)})
	if r.Outcome != FallbackError {
		t.Fatalf("want FallbackError, got %s", r.Outcome)
	}
	if r.Text != in {
		t.Errorf("error fallback must return raw text")
	}
	if r.Reason != "server_error" {
		t.Errorf("want sanitized reason server_error, got %q", r.Reason)
	}
	if calls != 3 {
		t.Errorf("want all 3 models tried, got %d", calls)
	}
}

func TestOutcomeFallbackShort(t *testing.T) {
	in := strings.Repeat("λέξη ", 100) // 500 runes
	var calls int
	r := CleanResult(context.Background(), "key", in, Options{caller: fakeCaller("μικρό", nil, &calls)})
	if r.Outcome != FallbackShort {
		t.Fatalf("want FallbackShort, got %s", r.Outcome)
	}
	if r.Text != in {
		t.Errorf("short fallback must return raw text")
	}
}

func TestOutcomeRawTooShort(t *testing.T) {
	var calls int
	r := CleanResult(context.Background(), "key", "   ", Options{caller: fakeCaller("x", nil, &calls)})
	if calls != 0 {
		t.Errorf("model must not be called for too-short input, got %d calls", calls)
	}
	if r.Outcome != RawTooShort {
		t.Fatalf("want RawTooShort, got %s", r.Outcome)
	}
}

func TestOutcomeFallbackDisabled(t *testing.T) {
	r := CleanResult(context.Background(), "", "hello there", Options{})
	if r.Outcome != FallbackDisabled || r.Text != "hello there" {
		t.Fatalf("no key must be FallbackDisabled with raw text, got %s %q", r.Outcome, r.Text)
	}
}

func TestOutcomeTruncatedRejected(t *testing.T) {
	in := strings.Repeat("λέξη ", 30)
	var calls int
	// Full-length output but ends on a dangling conjunction → treated as truncated.
	out := strings.Repeat("καθαρό ", 30) + "και"
	r := CleanResult(context.Background(), "key", in,
		Options{AllowShrink: true, caller: fakeCaller(out, nil, &calls)})
	if r.Outcome != FallbackShort {
		t.Fatalf("truncated output must be rejected, got %s", r.Outcome)
	}
}

func TestTotalDeadlineBounded(t *testing.T) {
	in := strings.Repeat("λέξη ", 30)
	var calls int
	slow := func(ctx context.Context, _, _ string) (string, error) {
		calls++
		select {
		case <-time.After(40 * time.Millisecond):
			return "", errors.New("boom")
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	longChain := make([]string, 20)
	for i := range longChain {
		longChain[i] = "m"
	}
	start := time.Now()
	r := CleanResult(context.Background(), "key", in,
		Options{Models: longChain, TotalDeadline: 150 * time.Millisecond, caller: slow})
	elapsed := time.Since(start)
	if r.Outcome != FallbackError {
		t.Fatalf("want FallbackError, got %s", r.Outcome)
	}
	if r.Attempts >= 20 {
		t.Errorf("attempts must be bounded by deadline, got %d", r.Attempts)
	}
	if elapsed > 600*time.Millisecond {
		t.Errorf("total elapsed %v exceeded deadline budget", elapsed)
	}
}

// Live test against the real API; skipped unless GEMINI_API_KEY is set.
func TestCleanLive(t *testing.T) {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		t.Skip("set GEMINI_API_KEY for the live cleanup test")
	}
	in := "λοιπον λοιπον πααμε να δουμε αν αν δουλευει, ε... ε... με commit και push"
	out, err := Clean(context.Background(), key, in, Options{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("live clean failed: %v", err)
	}
	if out == "" {
		t.Fatal("empty output")
	}
	if strings.Contains(out, "commit") == false {
		t.Errorf("English term should be preserved, got: %s", out)
	}
	t.Logf("cleaned: %s", out)
}

func TestAdHocPromptAugmentation(t *testing.T) {
	var seenPrompt string
	caller := func(ctx context.Context, model, prompt string) (string, error) {
		seenPrompt = prompt
		return "out", nil
	}
	in := "κείμενο προς καθάρισμα εδώ μέσα"
	_ = CleanResult(context.Background(), "key", in, Options{
		Body:   "BASE BODY",
		AdHoc:  "κάνε το πιο επίσημο",
		caller: caller,
	})
	if !strings.Contains(seenPrompt, "BASE BODY") {
		t.Errorf("base body missing from prompt")
	}
	if !strings.Contains(seenPrompt, "Έκτακτη οδηγία") || !strings.Contains(seenPrompt, "κάνε το πιο επίσημο") {
		t.Errorf("ad-hoc block missing from prompt:\n%s", seenPrompt)
	}
	// Ad-hoc block comes AFTER the base body (subordinate to the base rules).
	if strings.Index(seenPrompt, "BASE BODY") > strings.Index(seenPrompt, "Έκτακτη οδηγία") {
		t.Errorf("ad-hoc block should follow the base body")
	}
}

func TestNoAdHocBlockWhenEmpty(t *testing.T) {
	var seenPrompt string
	caller := func(ctx context.Context, model, prompt string) (string, error) {
		seenPrompt = prompt
		return "out", nil
	}
	_ = CleanResult(context.Background(), "key", "κάποιο κείμενο εδώ", Options{Body: "BASE", caller: caller})
	if strings.Contains(seenPrompt, "Έκτακτη οδηγία") {
		t.Errorf("ad-hoc block must be absent when AdHoc is empty")
	}
}
