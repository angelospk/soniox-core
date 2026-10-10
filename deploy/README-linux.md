# Running `soniox-core` on the mini PC (Linux)

The mini PC is the always-on speech boundary for Brain (D28): Brain calls
`http://127.0.0.1:8722`, never the Soniox provider directly.

One user unit does the work: `soniox-core.service`, the Go HTTP/WS service on
`127.0.0.1:8722`.

## One-time setup

```bash
ssh minipc
cd ~/projects/soniox-core

go build -o bin/soniox ./cmd/soniox

mkdir -p ~/.config/systemd/user
cp deploy/soniox-core.service ~/.config/systemd/user/
systemctl --user daemon-reload
```

The keys live in a `0600` env file of its own (D34):

```bash
install -d -m 700 ~/.config/soniox-core
install -m 600 /dev/null ~/.config/soniox-core/soniox.env
# SONIOX_API_KEY=...   (or SONIOX_KEY_COMMAND=..., see the README, "Soniox key")
# GEMINI_API_KEY=...   (cleanup and /review)
# stream them in, never type them into a shell that has history
```

### Enable

```bash
systemctl --user enable --now soniox-core.service
systemctl --user --no-pager status soniox-core.service
```

`loginctl enable-linger "$USER"` if the unit must survive you logging out.

## When the key fails

Brain's ops check notices a failing key and sends **one** Telegram alert —
`⚠️ Soniox key degraded` (D44). There is no repeat and no daily green message.
Meanwhile Brain keeps capturing voice through the **oc-asr fallback**, labelled
as a Degraded Voice Draft (D19/D20). Nothing is lost while you are asleep.

## Checking a cached key without printing it

With `SONIOX_KEY_COMMAND`, the key is cached in
`~/.cache/soniox-dictate/temp_key.json`:

```bash
node -e 'const fs=require("fs");const p=process.env.HOME+"/.cache/soniox-dictate/temp_key.json";
const s=fs.statSync(p),j=JSON.parse(fs.readFileSync(p,"utf8"));
console.log(JSON.stringify({mode:(s.mode&0o777).toString(8),hasKey:!!j.api_key,expires_at:j.expires_at}))'
```

Expect `mode: "600"`, `hasKey: true`, and an `expires_at` in the future. Never
`cat` this file, and never let it into a journal or a paste.

## Smoke test

```bash
curl --fail --silent http://127.0.0.1:8722/healthz          # -> ok
curl --fail --silent -F file=@testdata/greek-short.ogg \
     http://127.0.0.1:8722/transcribe | jq '{raw, model}'
curl --fail --silent -H 'content-type: application/json' \
     -d '{"sessionId":"smoke","revision":1,"text":"...","lang":"el","mode":"meaning-only"}' \
     http://127.0.0.1:8722/review | jq '[.findings[].kind] | unique'
```

The last command must only ever print `misheard` and/or `nonsense`. Use a
non-personal fixture — this output is fine to paste into a ticket, and personal
audio is not.

## Operate

```bash
journalctl --user -u soniox-core.service -n 50
```
