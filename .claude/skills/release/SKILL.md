---
name: release
description: Publish a new version of Futsal Cam Recorder so the Windows PC auto-updates. Use when the user asks to release, ship or deploy a change.
---

# Release

Installed PCs check GitHub `releases/latest` every hour (and one minute after
start-up). They download `futsal-cam-recorder.exe`, verify it against
`checksums.txt`, and swap it in **only when nothing is recording or being
finalized**. The Windows service then restarts on the new version.

## Steps

1. Make sure the change is merged into `development` and CI is green there.
2. Trigger the **Release** workflow (`.github/workflows/release.yml`) on `development` with `bump` = `patch` (fixes), `minor` (features) or `major` (breaking):
   - with the GitHub MCP tools, use `actions_run_trigger` (workflow `release.yml`, ref `development`, inputs `{"bump":"patch"}`), or
   - ask the user to go to GitHub → Actions → Release → Run workflow (this also works in the mobile app).
3. Watch the run until it finishes (`actions_get` / `actions_list`). It tests, cross-compiles, bundles ffmpeg.exe and publishes the tag and release with three assets: `futsal-cam-recorder.exe`, `futsal-cam-recorder-win-x64.zip` and `checksums.txt`.
4. Tell the user the version number and that the PC will pick it up within about an hour, once it is idle. In the UI footer, the version changes, and "Verificar atualizações" forces an immediate check.

## Changelog

The workflow generates the new section with [git-cliff](https://git-cliff.org) (config: `cliff.toml`, pinned to 2.14.2 in `release.yml`) from the conventional commits since the last tag. It inserts the section below the `CHANGELOG.md` header, commits it to `development` as `chore(release): X.Y.Z`, and uses the same text as the release notes. Only `feat`, `fix`, `perf`, `chore(deps)` and breaking changes are listed, so write commit subjects for humans. Run `git pull` after a release, because the workflow pushed a commit. To preview the next section: `pip install git-cliff==2.14.2`, then `git cliff --unreleased --tag vX.Y.Z --strip all`.

## If the release fails

- **"Bundle ffmpeg" step**: `scripts/fetch-ffmpeg.sh` finds the newest `ffmpeg-n*-latest-win64-lgpl-*.zip` (falling back to `master`) in the BtbN/FFmpeg-Builds `latest` release, and its error message lists the assets it saw. If BtbN renamed its assets, adjust the regex. The CI step "Check ffmpeg build is available" runs the same lookup on every push.
- A failed run publishes nothing (no tag, no release), so it is safe to re-run after fixing.
- **Tests**: fix them on a branch first. Never publish with failing tests: a broken binary would auto-install on the PC.

## Never

- Rename the assets, the repo, the tag format or the service name. Installed PCs would stop updating (see "Contracts" in CLAUDE.md).
- Publish a release whose exe can't start. The PC would be stuck until someone goes there. If in doubt, run the `run-app` skill first.
