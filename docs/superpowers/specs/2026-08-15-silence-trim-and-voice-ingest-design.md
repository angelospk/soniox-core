# Silence trimming + Telegram voice ingest — design

**Date:** 2026-08-15
**Status:** approved (brainstorm), pending implementation plan
**Repos touched:** `soniox-core` (phase 1 + 2a), `brain` on the mini PC (phase 2b)
**Base commit:** `b58eb32` (soniox-core)

## Goal

Two related things, in order:

1. **Phase 1 — make file transcription faster** by not paying real-time streaming
   cost for silence.
2. **Phase 2 — accept Telegram voice notes on the mini PC** and turn them into
   `brain capture` entries, so a spoken thought reaches the vault in one step.

Phase 2 depends on phase 1 only in spirit (same engine, same speed win); the two
can land independently, and each gets its **own implementation plan** — this
document is the shared design, not a single work item.

---

## Phase 1 — silence trimming in `soniox-core`

### Why the current path is slow

`transcribe.File()` splits a file into N spans and streams each to Soniox
concurrently. A single Soniox realtime stream runs at ~1x, so `feedPaced` sends
one 100 ms PCM chunk per 100 ms tick. Wall clock is therefore **the byte count of
the largest segment, in real time** — and silence costs exactly as much as speech.

Secondary waste: `ffprobe` for duration, one full `ffmpeg` pass for
`silencedetect`, then **one more `ffmpeg` decode per segment** (`N+2` processes).

### Corrections to the initial idea (from Codex plan review)

Three assumptions in the first sketch were wrong and are corrected here:

- **Job count must come from the *original* duration.** Recomputing
  `AutoJobs(trimmedDur)` cancels the win: a 10-minute file trimmed to 6 minutes
  would get 6 jobs instead of 10 and finish in the same wall clock. Below the
  `maxJobs` ceiling the speedup exists *only* if the job count is held.
- **Trimming does not rebalance segments.** The original claim ("silence-heavy
  segments finish early") is false — pacing is per byte, so an equal-duration
  silence-heavy segment takes equally long. The real imbalance comes from
  boundary snapping, which lets a cut move by up to `segLen/2`.
- **Boundary candidates and trim candidates are different thresholds.** Today
  `silencedetect` offers cuts from 0.4 s up. Using the trim threshold (0.6 s) for
  cuts would leave a file whose pauses are all 0.4–0.6 s with *no* candidates →
  a single span → parallelism collapses to 1.

### Design

Four independent knobs, deliberately separated:

| knob | default | meaning |
|---|---|---|
| `boundaryMin` | 0.4 s | a silent run at least this long is a candidate cut point |
| `trimMin` | 0.6 s | a silent run longer than this gets shortened |
| `keep` | 0.3 s | what remains of a trimmed run |
| `noiseDB` | −30 dBFS | the silence threshold |

Conservative capping, not removal: pause cues survive, so punctuation and
sentence segmentation are not starved.

**New pipeline:**

1. **One `ffmpeg` decode** of the whole file → s16le / mono / 24 kHz PCM into a
   temp file. Duration derives from file size (`len / 48000` bytes per second);
   the `ffprobe` pass disappears.
2. **`audio.TrimSilence`** streams that temp file into a second temp file,
   capping silent runs. It returns the trimmed length plus **boundary candidates
   on the trimmed timeline** — the midpoints of *every* surviving run ≥
   `boundaryMin`, including runs that were never trimmed.
3. **`segment.AutoJobs(originalDur, …)`** — computed from the original duration,
   then clamped by available boundaries, a minimum segment length, and `maxJobs`.
4. **`segment.Plan(trimmedDur, jobs, boundaries)`** — unchanged code.
5. **Segments are `io.SectionReader`s** over the shared trimmed temp file; each
   goroutine reads one paced chunk at a time. `audio.DecodePCM` leaves this path.

Net: `N+2` ffmpeg processes → **1**.

### Fail-open guard (required)

An absolute −30 dBFS threshold misclassifies a low-gain phone recording as
entirely silent, and "conservative capping" would then delete the user's words.
`TrimSilence` therefore **returns the original PCM untouched** when the result
looks implausible — specifically when the share of audio classified as silence
exceeds a ceiling (start at 80%), or when no run of speech is found at all. False
negatives cost speed; false positives must never cost words.

### Splice handling

A 5–10 ms fade-out/fade-in around each splice. Frame RMS below threshold does not
bound individual samples, so a hard cut can leave a DC step. Zero-crossing
alignment is unreliable with DC offset; a short fade is cheaper and more robust.
All offsets are integer samples internally; seconds appear only at external
interfaces, so no rounding gap or overlap can open between adjacent spans.

### API surface

- `transcribe.FileOptions` gains `Trim bool` and the thresholds.
- `/transcribe` accepts `?trim=0` to bypass; env vars override the thresholds.
- Library callers get the bypass through the option, not only over HTTP.

### Explicit non-goals

- **Live path unchanged.** In `/live` the silence is already free — you speak in
  real time — so trimming buys no latency, only complexity.
- **No timestamp contract.** `transcribe.File` returns merged text only; token
  timings are not exposed. Trimming breaks any mapping to original-file time, so
  if timestamps are ever exported this design must first add a piecewise
  trimmed→original sample map.
- **Pacing stays at 1x.** Feeding faster is a separate, complementary experiment;
  Soniox documents real-time cadence and may close bursty connections. Not
  bundled here.
- **`maxJobs` stays 18.** The in-repo comment records it as *measured* for one
  temp key. Not changed as a side effect of this work.

### Landing order

Four independently verifiable commits, not one:

1. Decode once, derive duration from PCM size, feed segments from file sections.
   No audio semantics change; `N+2` → 2 processes.
2. Add `TrimSilence` + its unit tests, unused.
3. Wire it behind `Trim`, **default off**, with `?trim=0` and env parsing.
4. **Flip to default on** after measuring on real recordings.

Step 4 is where the user's "default on" request lands. Enabling before
measurement would be enabling blind.

### Test set

Pure-function tests over synthetic PCM (no ffmpeg in tests):

- No silence; sub-threshold silence; leading/trailing silence; all silence; empty
  PCM; partial final frame; clip shorter than `keep`.
- Boundary values at 0.399/0.400 and 0.599/0.600/0.601 s.
- **Low-gain speech fails open** — a uniformly quiet recording comes back
  byte-identical.
- Boundary candidates survive when *no* run is trimmed.
- Invariants: even byte length, monotonic offsets, no reordered samples, spans
  cover the trimmed stream with no gap or overlap.
- `go test -race` on concurrent section feeding, including cancellation and temp
  file cleanup.
- A fake command runner proving exactly **one** ffmpeg invocation.
- HTTP tests: `trim=0`, invalid thresholds.

Before step 4, a measured comparison on a real corpus (silence-heavy speech,
continuous speech, low-gain phone audio, very short clips): transcript text,
punctuation, longest span, wall clock.

### Expected win — stated honestly

`removed = Σ max(0, runLength − keep)` over runs longer than `trimMin`. "40%
pause" does **not** imply 40% faster: pauses made of 0.5 s gaps yield zero. The
win is real for recordings with long thinking pauses and near zero for dense
continuous speech.

---

## Phase 2 — Telegram voice notes → `brain`

### Current state (verified 2026-08-15)

- Mini PC runs three Telegram bots as user services: `brain-telegram-bot`,
  `engram-bot`, `nightshift-telegram`.
- `brain-telegram-bot` handles **only** `callback_query` — it is an approve/reject
  bot with no message handler and no voice support at all.
- `brain capture` already accepts `--source` and `--dedup-key`, so an idempotent
  producer is supported out of the box.
- Mini PC has `ffmpeg`, `node`, Chrome 150, an X11/KDE session, `notify-send`.
  **No Go toolchain** — irrelevant, `soniox-core` is pure Go with no cgo and
  cross-compiles from the Mac.
- `~/.cache/soniox-dictate/temp_key.json` **exists but is a dead leftover**: it
  expired at 08:04Z and no timer or service writes it.
- `oc-asr.service` (faster-whisper) is running on `127.0.0.1:8000`.
- `GEMINI_API_KEY` is **not** present on the mini PC (it lives in
  `soniox-dictate/.env` on the Mac). `soniox-core` reads it straight from the
  environment.

### Scope

Only `brain-telegram-bot`. The voice→text step goes in a shared module so
`engram-bot` and `nightshift-telegram` can adopt it later, but nothing is designed
for three bots before one works.

### Flow

1. A voice/audio message arrives from the owner. Non-owner messages are ignored,
   matching the existing authorization check.
2. The bot downloads the OGG/Opus file.
3. It POSTs to local `soniox-core` `/transcribe` — same engine, same prompt
   profiles, same vocabulary as desktop dictation, and now with silence trimming.
4. Cleanup runs with the existing **`notes`** profile.
5. The bot replies with the cleaned text and three buttons: **✅ capture**,
   **✏️ διόρθωση**, **❌ άκυρο**.
6. ✅ runs `brain capture --source telegram-voice --dedup-key <file_unique_id>`.
   The dedup key makes a double tap or a retry a no-op.

The confirmation tap is deliberate: the goal is fast ingest, but a mis-heard note
landing silently in the vault is worse than one tap.

### Key supply — decision K3

`soniox-core` needs a Soniox key on the mini PC.

**Primary — a key source on the mini PC itself.** Same setup as the Mac:
`SONIOX_API_KEY`, or a short-lived key kept fresh in the key cache (see the
README, "Soniox key").

**Fallback — `oc-asr`.** When no valid key is available, the bot falls back to
the already-running faster-whisper service instead of failing. The reply marks
the degraded result so a bad transcript is never mistaken for the good path. This
changes the failure mode from *dead* to *degraded* for a small amount of extra
work.

Rejected: relaying a short-lived key from the Mac. A closed laptop would kill
the bot once that key expires, which is precisely the "out of the house, sending
a voice note" case that motivates the feature.

### Secrets to provision (manual, owner's decision)

Both are the user's own keys moving between the user's own machines, but neither
should be copied silently:

- `GEMINI_API_KEY` → a systemd `EnvironmentFile` for the mini PC `soniox-core`
  unit. Without it, cleanup is skipped and raw text is captured.
- A Soniox key source on the mini PC (see the README, "Soniox key").

### Phase 2 open risk

The key supply is the fragile part. The `oc-asr` fallback exists so that a
missing key degrades quality instead of taking the feature offline. If the key
supply proves unmaintainable in practice, the fallback becomes the primary and
the design question reopens.

---

## Success criteria

**Phase 1**

- Test suite above passes, including `-race`.
- Exactly one ffmpeg process per file transcription.
- On a silence-heavy real recording, measured wall clock drops materially versus
  `?trim=0`, with transcript text and punctuation reviewed as unchanged in
  substance.
- A low-gain recording returns byte-identical audio (guard fires).

**Phase 2**

- A Telegram voice note to `brain-telegram-bot` returns cleaned Greek text.
- ✅ creates exactly one vault entry; a second tap creates none.
- With the Soniox key deliberately invalidated, the bot still replies via
  `oc-asr` and marks the result as degraded.
