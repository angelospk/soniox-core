package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/angelospk/soniox-core/internal/cleanup"
	"github.com/angelospk/soniox-core/internal/config"
	"github.com/angelospk/soniox-core/internal/weekly"
)

// reportDir holds one JSON report per weekly review run.
func reportDir() string { return filepath.Join(prefsDir(), "weekly-review") }

// cmdWeeklyReview proof-reads the last N days of dictations in chunks, ranks the
// recurring findings, drops ones that already have a rule, saves the report, and
// prints it as a markdown table for a person to pick from.
func cmdWeeklyReview(args []string) {
	fs := flag.NewFlagSet("weekly-review", flag.ExitOnError)
	days := fs.Int("days", 7, "how many days of dictations to review")
	sessions := fs.String("sessions", filepath.Join(prefsDir(), "dictations", "sessions.jsonl"), "sessions.jsonl")
	limit := fs.Int("limit", 30, "max items to report (most frequent first)")
	_ = fs.Parse(args)

	key := config.GeminiKey()
	if key == "" {
		fmt.Fprintln(os.Stderr, "weekly-review: GEMINI_API_KEY not set (~/.soniox-core/.env)")
		os.Exit(1)
	}
	texts, err := weekly.LoadTexts(*sessions, time.Now().AddDate(0, 0, -*days))
	if err != nil {
		fmt.Fprintln(os.Stderr, "weekly-review:", err)
		os.Exit(1)
	}
	vocab := splitLines(readVocab())
	models := cleanup.ReadModelChain(filepath.Join(prefsDir(), "cleanup-models"))
	// 12 KB: big enough to batch many short dictations, small enough that a
	// flash-lite model answers well inside the batch deadline.
	chunks := weekly.Chunks(texts, 12<<10)
	failed := 0
	review := func(ctx context.Context, text string) ([]cleanup.Finding, error) {
		f, _, err := cleanup.ReviewBatch(ctx, key, text, "", vocab, models, nil)
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "weekly-review: chunk failed: %v\n", err)
		}
		return f, err
	}
	items := weekly.Aggregate(weekly.Collect(context.Background(), chunks, review))
	items = weekly.DropKnown(items, filepath.Join(prefsDir(), "corrections.tsv"))
	if len(items) > *limit {
		items = items[:*limit]
	}
	path := filepath.Join(reportDir(), time.Now().Format("2006-01-02")+".json")
	if err := weekly.SaveReport(path, items); err != nil {
		fmt.Fprintln(os.Stderr, "weekly-review:", err)
		os.Exit(1)
	}
	fmt.Printf("dictations: %d · chunks: %d (failed: %d) · new findings: %d\nreport: %s\n\n",
		len(texts), len(chunks), failed, len(items), path)
	fmt.Print(weekly.Markdown(items))
}

// cmdApplyReview writes the chosen report items to corrections.tsv (and, with
// -vocab, their replacement terms to vocabulary.txt).
func cmdApplyReview(args []string) {
	fs := flag.NewFlagSet("apply-review", flag.ExitOnError)
	report := fs.String("report", "", "report JSON (default: the newest in ~/.soniox-dictate/weekly-review)")
	ids := fs.String("ids", "", "comma-separated item ids to add as correction rules")
	vocabIDs := fs.String("vocab", "", "comma-separated item ids whose replacement goes to vocabulary.txt")
	_ = fs.Parse(args)

	path := *report
	if path == "" {
		path = newestReport()
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "apply-review: no report found; run weekly-review first")
		os.Exit(1)
	}
	items, err := weekly.LoadReport(path)
	if err == nil {
		var a, v []int
		if a, err = parseIDs(*ids); err == nil {
			if v, err = parseIDs(*vocabIDs); err == nil {
				var n int
				n, err = weekly.Apply(items, a, v,
					filepath.Join(prefsDir(), "corrections.tsv"), filepath.Join(prefsDir(), "vocabulary.txt"))
				if err == nil {
					fmt.Printf("added %d line(s) from %s\n", n, path)
					return
				}
			}
		}
	}
	fmt.Fprintln(os.Stderr, "apply-review:", err)
	os.Exit(1)
}

func newestReport() string {
	m, _ := filepath.Glob(filepath.Join(reportDir(), "*.json"))
	if len(m) == 0 {
		return ""
	}
	sort.Strings(m) // names are dates
	return m[len(m)-1]
}

func parseIDs(s string) ([]int, error) {
	var out []int
	for _, p := range splitComma(s) {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("bad id %q", p)
		}
		out = append(out, n)
	}
	return out, nil
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
