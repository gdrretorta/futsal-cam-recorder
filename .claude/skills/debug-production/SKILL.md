---
name: debug-production
description: Investigate a problem reported on the installed recorder (failed recording, camera not connecting, update not applied) without physical access to the PC. Use when the user reports a bug seen in the live app.
---

# Debug a problem on the installed PC

Nobody can easily go to the PC, so start from what the app exposes remotely.

## 1. Gather evidence (read-only)

Ask the user for the following, or fetch it yourself if the sandbox can reach the site. Only use the read-only `GET` endpoints, and never start, stop or delete anything in production.

- `https://futsal.retorta.com/api/status`: recorder state, last error, version, update state, free disk.
- The logs: the last 500 log lines (ffmpeg errors are prefixed `ffmpeg:`). They are admin-only (`GET /api/logs` returns 401 without a session), so ask the user to log in and paste them from the "Logs" link in the footer.
- `https://futsal.retorta.com/api/recordings`: file sizes. A tiny file usually means the camera dropped.

Cloudflare Access may block automated requests. If it does, ask the user to paste the output.

## 2. Common causes

| Symptom | Where to look |
|---|---|
| `não foi possível ligar à câmara: ...` | The camera IP, path or credentials, or the camera is offline. "Testar ligação" in the settings (admin login needed) shows the ffmpeg error. |
| Stuck in `reconnecting` | The camera or network is dropping. The stall detector (`stallTimeout`) and the `-timeout` input option are in `recorder.go`. |
| `Malformed AAC bitstream` / `dimensions not set` in the logs | Camera stream quirks (ADTS audio, long keyframe interval). Handled by `-bsf:a aac_adtstoasc` and the larger `-analyzeduration` in `recorder.go`. |
| Recording plays badly or can't seek | Finalization failed. Look for "finalize failed" in the logs: the file stays fragmented MP4, which is still playable. |
| Video has hiccups (freezes, jumps) | The `recording stats` log line written when each file is finalized: `fps` below the camera's setting or `gaps` > 0 means the camera skipped frames, usually because its Wi-Fi can't keep up with `kbps`. Fix it on the camera (lower bitrate, constant bitrate) or the Wi-Fi, not in ffmpeg. |
| Update not applied | `update.state` is `waiting` while recording or finalizing (expected). On `error`, the message says why. A `dev` version never updates. |
| "ainda não existe nenhum utilizador" | No user yet (the recorder is open to everyone until then). Run `install.ps1` again on the PC, or `add-user <email> admin` there. It can't be done remotely, by design. |
| Forgotten password | Another admin (or a userManager, for non-admins) can set a new one in the users page. Otherwise, on the PC: `'<pw>' \| .\simple-rtsp-recorder.exe add-user <email> <role>`. |
| Locked out ("demasiadas tentativas") | 5 failed logins from that IP. Wait 15 minutes, or restart the service. |
| Public site looks unstyled or outdated, but `localhost:8080` on the PC is fine | Cloudflare is caching an old asset. Check that `index.html` references every asset with `?v=<hash>` (`internal/server/static.go`). Immediate workaround: Cloudflare dashboard → Caching → Purge Everything. |
| Old version still shown after an update | The service did not restart. Check the recovery actions in `service.Install`. Running `install.ps1` again on the PC re-installs the service. |

## 3. Reproduce locally

Use the `run-app` skill: the fake camera plus `pkill -x mediamtx` simulates a camera drop, and killing the app simulates a restart. Write a failing test in `internal/recorder/recorder_test.go` or `internal/server/server_test.go` before fixing.

## 4. Ship the fix

Open a PR, get CI green, merge it, then use the `release` skill with `bump: patch`. The PC installs the fix by itself once it is idle.
