# CLAUDE.md

Simple RTSP Recorder records one RTSP camera (a futsal pitch) to MP4 files on a
Windows PC on the same LAN as the camera. It is controlled from a web page that is
published at https://futsal.retorta.com through a Cloudflare Tunnel. The owner
can't easily get to the PC, so **auto-update is critical**: never break it.

## Stack

- Go (stdlib only, plus `golang.org/x/sys` for the Windows service and disk APIs), one static `.exe`.
- ffmpeg does the recording (`-c copy`, no re-encode). A Windows ffmpeg.exe is shipped next to the exe in the release zip.
- The web UI is plain HTML/CSS/JS with no build step, embedded with `go:embed`. UI text is **Portuguese (pt-PT)**; code, comments and logs are English.

## Layout

```
main.go                     commands (run/install/uninstall/version), wiring, restart after update
internal/config             config.json load/save/validate; migration from 1.x prod.env
internal/recorder           ffmpeg process: start/stop, reconnect, finalize (faststart remux), snapshot, list/prune
internal/server             HTTP API + static files (endpoint list in the package doc); auth.go = login, sessions, roles; users.go = user management
internal/updater            GitHub release check → download + sha256 verify → swap exe when idle → restart
internal/service            Windows service run/install/uninstall (stub on other OSes)
internal/notify             emails to the admins through the Resend API; Watcher = long-recording alert
internal/logging            slog → rotating file logs/recorder.log + stdout + in-memory tail for /api/logs
internal/sysinfo            free disk space (windows / unix)
web/                        index.html, app.js, style.css, icon.svg (embedded)
scripts/fake-camera.sh      local RTSP test camera (mediamtx + ffmpeg test pattern)
install.ps1                 one-time installer/upgrader (irm | iex)
.github/workflows           ci.yml (fmt, vet linux+windows, test -race, build), release.yml
```

Runtime files in the install folder: `simple-rtsp-recorder.exe`, `ffmpeg.exe`,
`config.json`, `recording.state` (present while a recording should be running, so it
resumes after a restart), `session.key` (signs login sessions), `logs/`, `recordings/`.

## Contracts that must not change

Installed PCs depend on these. Changing any of them strands the PC on the old version:

- Release assets are named `simple-rtsp-recorder.exe`, `checksums.txt` (sha256sum format) and `simple-rtsp-recorder-win-x64.zip`. See `updater.AssetName` and `release.yml`.
- The repo is `joaoopereira/simple-rtsp-recorder` and releases use semver tags `vX.Y.Z` (`updater.Newer`).
- The service name is `SimpleRTSPRecorder`. Restart after an update relies on the SCM recovery actions set in `service.Install`, together with a non-zero exit from the service.
- `config.json` stays backward compatible: only add fields, with defaults in `config.Default()`.
- Recording file names follow `2006-01-02_150405.mp4`. The UI parses the date from the name.

## Commands

```bash
gofmt -l .                          # must print nothing
go vet ./... && GOOS=windows go vet ./...   # always vet the windows build too
go test -race ./...                 # recorder tests need ffmpeg on PATH (skipped otherwise)
node --check web/app.js
GOOS=windows GOARCH=amd64 go build -o simple-rtsp-recorder.exe .
```

`go.mod` requires Go 1.26 (needed by `golang.org/x/sys`). If the installed Go is older, run commands with `GOTOOLCHAIN=auto`, which downloads the right toolchain from the Go proxy.

To run the app against the fake camera, see `.claude/skills/run-app/SKILL.md`.

## Conventions

- Conventional commits (`feat:`, `fix:`, `chore:` …). The default branch is `development`. The owner wants changes pushed straight to `development`: don't open pull requests unless asked. `CHANGELOG.md` and the release notes are generated during the release by git-cliff (`cliff.toml`), from `feat`/`fix`/`perf`/`chore(deps)` subjects and `BREAKING CHANGE:` footers. Don't edit `CHANGELOG.md` by hand for new versions.
- Prefer the standard library. Don't add a frontend framework or build step.
- The recorder's state machine is `idle → connecting → recording ⇄ reconnecting → stopping → idle`. All state lives under `Recorder.mu`. Don't call ffmpeg while holding the lock.
- Never log credentials: use `config.Redact(url)`. The camera password is write-only in the API: `GET /api/settings` returns `hasPassword`, and `PUT` keeps the stored password when `password` is absent.
- Validate file names with `recorder.ValidName` before touching the disk (prevents path traversal).
- Windows-only code goes in `_windows.go` files, with a `_other.go` counterpart (`//go:build !windows`).
- User-facing error messages are Portuguese, because they are shown in the UI as they are.
- A Windows service has **no console**: writes to stdout/stderr fail. Never let a stdout failure stop other output (that is why `logging` uses `allWriters` instead of `io.MultiWriter`; in v2.0.x the "Registos" page was empty because of it), and never rely on console output for anything the service needs.
- The site sits behind Cloudflare, which caches `.css`, `.js` and `.svg` by extension. `index.html` is served with `Cache-Control: no-store`, and every asset it references is rewritten to `name?v=<content hash>` (`internal/server/static.go`). A new file in `web/` must be added both to `//go:embed` in `web/embed.go` and to `assets` in `static.go`. Never reference an asset without the hash: in v2.0.0, Cloudflare kept serving the 1.x `style.css`, and the public page rendered unstyled.

## Adding things

- **API endpoint**: register a handler in `server.Handler()`, add it to the package doc list, and add a test in `server_test.go`. Then call it from `web/app.js` through `api()`.
- **Setting**: add the field to `config.Config` with a default and validation, then to `settingsDTO` and its `toDTO`/`apply` in `server.go`, then to the settings form in `index.html` and to `openSettings`/`formSettings` in `app.js`. Settings are admin-only automatically. `apply` must keep fields that aren't in the DTO, such as `users`.
- **Email alert**: build the message in `internal/notify` and send it with `notify.Send` to `notify.AdminEmails(cfg)`. Emails go out through the Resend HTTP API (`api.resend.com`), configured in the settings (`notify`). The long-recording alert (`notify.Watcher`, started in `main.go`) checks every minute and emails the admins when a recording reaches `notify.longRecordingHours`, then every hour; a failure is retried after 10 minutes. It counts from `Recorder.Status().StartedAt`, which survives a restart (the start time is kept in `recording.state`).
- **ffmpeg arguments**: `recordArgs` / `inputArgs` in `recorder.go`. Cover the change in `recorder_test.go`, which uses a real ffmpeg and a generated test file.

## Security model

- **Login with email and password; three roles** (`config.Role`): `admin` (everything), `userManager` (recorder + managing plain users), `user` (recorder only). Users live in `config.json` (`users`: id, email, role, PBKDF2-SHA256 `passwordHash`, `mustChangePassword`).
- **Every endpoint is wrapped in `s.require(level, h)`** (`internal/server/auth.go`): `accessRecorder` (any user), `accessUsers` (admin, userManager), `accessAdmin` (settings, logs, update check) or `accessAccount` (only `PUT /api/me/password`, see below). A new endpoint must pick one. The package doc in `server.go` lists the level of each endpoint. The UI hides what the role can't use, but the server is what enforces it.
- **Public on purpose:** `GET /recordings/{name}`, so a recording's link can be shared without logging in (the owner asked for this). Also `GET /api/status`, but without access it returns only `{title, version, loginRequired}`: a page left open across an update must still see the new version and reload.
- **On the PC itself, the recorder needs no login** (the owner asked for this): a browser at `http://localhost:8080` without a session is a plain `user` (`localUser`, `Server.TrustLocal`, `isLocal` in `auth.go`). Settings and users still need an admin login, also on the PC (the header button logs in there; a session always wins over the local user). Never make the local user more than a `user`. cloudflared runs on the same PC, so tunnel traffic also comes from loopback: `isLocal` also needs no Cloudflare headers (`Cf-Connecting-Ip`, `Cf-Ray`…) and a localhost Host (which also stops DNS rebinding), and a change (non-GET) needs the page's own `Origin`/`Sec-Fetch-Site` so other web pages open on the PC can't use it (CSRF). Never relax these checks. Tests run with `TrustLocal` off, because httptest connects from loopback.
- **No users = open mode.** While `users` is empty, `accessRecorder` endpoints are open to everyone (as before 2.7) and nothing else is reachable. That way an automatic update can never lock the owner out of the PC.
- **Upgrade from ≤ 2.6:** the old `adminPasswordHash` becomes an admin user (`config.MigratedAdminEmail`, same password) when `config.json` is loaded (`migrateUsers`), and is then cleared.
- **The first admin is never created from the UI**, because the site is public and the first visitor would become admin. It is created on the PC, either by `install.ps1` (which asks for email and password when there is no admin) or with `echo <pw> | simple-rtsp-recorder.exe add-user <email> admin`. `set-admin-password` still works (it sets the first admin's password).
- **User management rules** (`canManage` in `users.go`, plus `config.Store`): only admins create, edit, promote to or delete admins and userManagers; a userManager only manages plain users (the owner asked for this). Nobody can delete their own account. The last admin can't be deleted or demoted. Changing your own password needs the current one, and the new one must differ.
- **Password chosen by someone else must be changed.** Creating a user from the UI, or resetting someone else's password, sets `mustChangePassword`. Until the user picks their own (`PUT /api/me/password`, level `accessAccount`), `require` answers 403 to everything else and `/api/status` returns the public answer; the page shows the "Escolha a sua palavra-passe" screen. Users made with `add-user` on the PC don't get the flag.
- Sessions use an HMAC-signed cookie (`srr_session` = `<user id>.<expiry>.<sig>`, HttpOnly, SameSite=Strict, Secure behind the tunnel). The signature covers the user's password hash, and the user is looked up on every request: a password change logs that user out elsewhere, and a role change or deletion applies at once. Sessions last 7 days and are renewed on every page load (`GET /api/me`). The signing key is kept in `session.key` in the install folder, so sessions survive restarts and updates; delete it to log everyone out.
- Failed logins are locked out per IP: 5 failures in 15 minutes. The IP comes from `Cf-Connecting-Ip`, trusted only when the connection comes from loopback (cloudflared). An unknown email takes as long as a wrong password (`dummyHash`).
- Changes to users go through `Store.Edit` (read-modify-write under a lock), so concurrent edits don't overwrite each other. `PUT /api/settings` uses it too.
- `config.Store` reloads `config.json` when the file changes on disk, so edits from the CLI or by hand apply without restarting the service.
- Never return the camera password, the Resend API key (`notify.resendApiKey`, write-only like the camera password: `hasApiKey` / absent `apiKey` keeps it) or a password hash from the API.
