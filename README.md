# soniox-core

A fast, concurrent **Go** core for the Soniox dictation stack — the engine behind
the [soniox-dictate](https://github.com/angelospk/soniox-dictate) tool, rebuilt to
be performant, dependency-light, and runnable **as a server**.

Built on the [moxierobots/soniox-stt-go](https://github.com/moxierobots/soniox-stt-go)
SDK (real-time `stt-rt-v5` over WebSocket) — we don't reimplement the protocol.

## Where this sits (three repos)

- **soniox-core** (this) — the Go server and **the only place AI cleanup + prompts
  live**: `internal/cleanup/` (`cleanup.go`, `profiles.go`) and `internal/server/server.go`
  (`cleanLive`). Tune prompts against real transcripts with `cmd/evalcleanup`.
- **[dictation-bar](https://github.com/angelospk/dictation-bar)** — the Swift menu-bar
  client (mic, paste, transcript history window, `sessions.jsonl`).
- **[soniox-dictate](https://github.com/angelospk/soniox-dictate)** — older sibling; holds
  `GEMINI_API_KEY` in its `.env`.

Shared state in `~/.soniox-dictate/` (read **per request**, so prompt/profile edits
apply with no restart; a binary rebuild needs a restart): `prompts/*.md`,
`active-profile`, `output-language`, `vocabulary.txt`, `dictations/sessions.jsonl`.
Cleanup outcomes are logged one line each (`outcome=…`) and surfaced to the client in
the `done` event + a `cleanup_failed` advisory. See dictation-bar's README for the full
websocket and `sessions.jsonl` schemas.

## What it does

- **File transcription**, parallel: splits the file at silences and transcribes
  the segments concurrently (capped at 18 — the measured per-key ceiling), then
  merges them in order. Wall time ≈ duration / jobs.
- **Live transcription server**: stream PCM to a WebSocket and get tokens back.
- **LLM cleanup** with a **model fallback chain**: light correction that keeps
  your words/voice/language. Tries fast Gemini-lite models first, then `gemma-4-31b-it`
  as a last resort — each is a separate quota bucket, so a `429`/`5xx` on one falls
  through to the next instead of silently pasting raw. Gemma config is model-aware
  (no `thinkingConfig`) and its "thought" parts are stripped from output. Edit the
  chain in `~/.soniox-dictate/cleanup-models` (no rebuild). Every cleanup logs one
  line (`outcome=… model=…`); on total failure the client is notified, not fooled.
- **Keys** (no Python anywhere): `SONIOX_API_KEY` (your own, from
  [console.soniox.com](https://console.soniox.com)). `GEMINI_API_KEY` enables
  cleanup. Check resolution with `soniox tempkey`.

### Soniox key

Set `SONIOX_API_KEY` and you are done. That is the supported setup.

If you rotate short-lived keys instead, leave `SONIOX_API_KEY` unset and point
`SONIOX_KEY_COMMAND` at a command of your own. The server resolves a key in this
order:

1. `SONIOX_API_KEY` (or `SONIOX_TEMP_KEY`)
2. the cached key in `~/.cache/soniox-dictate/temp_key.json`, while it has more
   than 90 s left
3. `SONIOX_KEY_COMMAND`, run through `/bin/sh -c` (60 s timeout)

The command prints either a bare key, or JSON:

```json
{"api_key": "…", "expires_at": "2026-01-01T12:00:00Z"}
```

A key with an RFC3339 `expires_at` is cached in the file above until it expires; a
bare key is not cached (it also clears any older cached key), so the command runs
on every connect. On failure, exit non-zero; to give the client
a hint, print `{"hint": "…"}` first. `/reconnect` re-runs the command even when the
cached key is still fresh. Anything else that writes the cache file (same JSON
shape, mode 0600) also works.

## Build

```bash
make build     # -> bin/soniox (needs ffmpeg/ffprobe on PATH)
```

## Quick start on a new machine

```bash
brew install go ffmpeg
mkdir -p ~/.soniox-core
printf 'SONIOX_API_KEY=...\nGEMINI_API_KEY=...\n' > ~/.soniox-core/.env  # your keys
make install      # builds + runs the server as a LaunchAgent on 127.0.0.1:8722
```
Then install the [dictation-bar](https://github.com/angelospk/dictation-bar) menu-bar
app.

**Re-run `make install` after every Go change.** The LaunchAgent runs the prebuilt
`bin/soniox`, so a fix that is merged but not rebuilt simply does not run — and the
symptom can be a bug you already fixed weeks ago reappearing in the wild. Check with
`stat -f %Sm bin/soniox` against the commit date when a fix seems to have no effect.

## CLI

```bash
soniox file recording.m4a --lang el,en        # transcribe -> stdout (cleaned)
soniox file --jobs 12 recording.m4a           # force parallelism
soniox serve --addr :8722                      # run the service
```

## Server endpoints

| route | method | what |
|---|---|---|
| `/healthz` | GET | liveness |
| `/live` | WS | stream binary PCM (s16le mono 24kHz) frames; receive JSON `{type: partial\|final\|done, text}`; send text `"done"` to finalize. `?lang=el&lang=en` to override. |
| `/transcribe` | POST | multipart `file` → JSON `{raw, cleaned, jobs, segments, duration_s, profile, lang, model, cleanup}`; uses the active shared cleanup preferences |
| `/clean` | POST | JSON `{text, profile, lang, adhoc}` → run the cleanup chain over already-transcribed text |
| `/alternatives` | POST | JSON `{text, items, lang}` → alternative phrasings for the given spans |
| `/reconnect` | POST | force a fresh Soniox key → `{ok, source, hint}`. A no-op success with `SONIOX_API_KEY`; otherwise re-runs `SONIOX_KEY_COMMAND` |

## Layout

```
cmd/soniox          CLI + server entrypoint
internal/config     env/.env, key resolution + cache, SONIOX_KEY_COMMAND
internal/audio      ffmpeg/ffprobe: duration, silence detection, PCM decode
internal/segment    silence-snapped parallel-cut planning
internal/soniox     SDK wrapper + partial/final text assembler
internal/cleanup    Gemini cleanup (brace-safe prompt, length guard, retry)
internal/transcribe parallel file engine (decode → streams → merge → cleanup)
internal/server     /healthz, /live (WS), /transcribe, /clean, /alternatives, /reconnect
```

## Tests

```bash
go test ./...                                   # unit tests (no network)
# live tests (real API), opt-in via env:
SONIOX_TEMP_KEY=... GEMINI_API_KEY=... TEST_AUDIO=clip.m4a go test ./...
```
