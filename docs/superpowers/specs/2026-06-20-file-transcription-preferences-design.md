# File Transcription Preferences Design

**Date:** 2026-06-20

## Goal

Make `POST /transcribe` apply the same shared AI-cleanup preferences as live
dictation so the DictationBar file picker produces consistent output.

## Behavior

For every request, the endpoint:

- resolves the current Soniox key;
- reads vocabulary from the shared preferences directory;
- checks `no-ai-cleanup`;
- loads the active prompt profile;
- reads the configured output language;
- runs the existing cleanup model fallback chain when cleanup is enabled;
- returns raw text, final text, transcription statistics, and cleanup metadata.

The active profile applies to the complete file. Spoken profile-switch commands
are intentionally unsupported for file transcription.

## API

Existing response fields remain unchanged:

```json
{
  "raw": "...",
  "cleaned": "...",
  "jobs": 4,
  "segments": 4,
  "duration_s": 120.5
}
```

The endpoint adds fields already conceptually aligned with the live `done`
event:

```json
{
  "profile": "default",
  "lang": "el",
  "model": "...",
  "cleanup": {
    "outcome": "cleaned",
    "reason": "",
    "elapsedMs": 850
  }
}
```

These additions are backward-compatible for existing clients.

## Boundaries

The transcription engine remains responsible only for media decoding,
segmentation, Soniox calls, and merging raw text. Preference resolution and
profile-aware cleanup belong at the server layer, where the shared preferences
directory is available and live cleanup already follows the same policy.

No streaming progress protocol, queued jobs, or endpoint cancellation API is
added.

## Error Handling

- Transcription errors retain the existing non-2xx behavior.
- Cleanup failure does not discard a valid raw transcript. The endpoint returns
  raw text as the final text with a fallback cleanup outcome.
- Disabled cleanup returns raw text with a disabled outcome.
- Empty transcription returns an explicit outcome and no fabricated text.

## Testing

- Active profile and output language affect file cleanup options.
- `no-ai-cleanup` skips cleanup.
- Cleanup failure preserves raw transcription.
- Existing response fields remain present.
- Cleanup metadata is returned for cleaned, disabled, fallback, and empty
  outcomes.
