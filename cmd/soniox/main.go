// Command soniox is the CLI/server entrypoint for the transcription core.
//
//	soniox file <audio>           transcribe a file (parallel) -> stdout
//	soniox serve [--addr :8722]   run the HTTP/WS service
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/angelospk/soniox-core/internal/cleanup"
	"github.com/angelospk/soniox-core/internal/config"
	"github.com/angelospk/soniox-core/internal/server"
	"github.com/angelospk/soniox-core/internal/transcribe"
)

// prefsDir is the shared prefs dir (~/.soniox-dictate) used by the menu-bar
// client too. Empty if the home dir can't be resolved.
func prefsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".soniox-dictate")
}

func loadEnv() {
	_ = config.LoadDotEnv(".env")
	if home, err := os.UserHomeDir(); err == nil {
		_ = config.LoadDotEnv(filepath.Join(home, ".soniox-core", ".env"))
	}
}

// readVocab loads ~/.soniox-dictate/vocabulary.txt (shared with the other tools).
func readVocab() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, ".soniox-dictate", "vocabulary.txt")) // #nosec G304 -- fixed path under $HOME
	if err != nil {
		return ""
	}
	var lines []string
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" && !strings.HasPrefix(ln, "#") {
			lines = append(lines, ln)
		}
	}
	return strings.Join(lines, "\n")
}

func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func main() {
	loadEnv()
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "file":
		cmdFile(os.Args[2:])
	case "serve":
		cmdServe(os.Args[2:])
	case "tempkey":
		key, err := config.ResolveSonioxKey(context.Background())
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(key)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:\n  soniox file <audio> [--lang el,en] [--jobs N] [--no-clean]\n  soniox serve [--addr :8722] [--lang el,en]")
}

func cmdFile(args []string) {
	fs := flag.NewFlagSet("file", flag.ExitOnError)
	lang := fs.String("lang", "el,en", "comma-separated language hints")
	jobs := fs.Int("jobs", 0, "parallel segments (0 = auto)")
	noClean := fs.Bool("no-clean", false, "skip the Gemini cleanup pass")
	// accept the path either before or after the flags
	var path string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		path, args = args[0], args[1:]
	}
	fs.Parse(args)
	if path == "" {
		path = fs.Arg(0)
	}
	if path == "" {
		usage()
		os.Exit(2)
	}

	ctx := context.Background()
	key, err := config.ResolveSonioxKey(ctx)
	if err != nil {
		log.Fatal(err)
	}
	res, err := transcribe.File(ctx, path, transcribe.FileOptions{
		Key:       key,
		Langs:     splitComma(*lang),
		Jobs:      *jobs,
		Cleanup:   !*noClean,
		GeminiKey: config.GeminiKey(),
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "[%.0fs audio · %d segments · %d jobs]\n",
		res.Duration, res.Segments, res.Jobs)
	fmt.Println(res.Cleaned)
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	// bind localhost by default — this is a personal tool, not a network service
	addr := fs.String("addr", "127.0.0.1:8722", "listen address")
	lang := fs.String("lang", "el,en", "comma-separated language hints")
	noClean := fs.Bool("no-clean", false, "disable cleanup for /transcribe and /live")
	lowconf := fs.Float64("lowconf", 0.5, "flag /live words below this STT confidence (0..1)")
	fs.Parse(args)
	if *lowconf < 0 || *lowconf > 1 {
		log.Fatalf("-lowconf must be in [0,1], got %v", *lowconf)
	}

	pdir := prefsDir()
	// seed the prompt profiles so the menu-bar client finds them on first run
	if pdir != "" {
		_, _ = cleanup.LoadProfiles(filepath.Join(pdir, "prompts"))
	}
	srv := &server.Server{
		KeyResolver:      config.ResolveSonioxKey,
		Reconnect:        config.ForceRefreshSonioxKey,
		GeminiKey:        config.GeminiKey(),
		Langs:            splitComma(*lang),
		Cleanup:          !*noClean,
		LowConfThreshold: *lowconf,
		Vocab:            readVocab(),
		PrefsDir:         pdir,
	}
	httpSrv := &http.Server{
		Addr:    *addr,
		Handler: srv.Handler(),
		// guard against slow-header (Slowloris) DoS; does not limit the WS body
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Printf("soniox-core listening on %s (GET /healthz, WS /live, POST /transcribe)", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	log.Print("shutting down…")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(sctx)
}
