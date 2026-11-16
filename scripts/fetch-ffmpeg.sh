#!/usr/bin/env bash
# Gets the Windows ffmpeg.exe bundled in the release zip.
#
#   scripts/fetch-ffmpeg.sh <out-dir>            # <out-dir>/ffmpeg.exe (+ FFMPEG-LICENSE.txt), pinned
#   scripts/fetch-ffmpeg.sh --check              # the pinned source still exists (CI)
#   scripts/fetch-ffmpeg.sh --latest <out-dir>   # newest BtbN/FFmpeg-Builds LGPL build, prints its sha256
#
# ffmpeg.exe runs as SYSTEM on the PC, so a release never takes whatever is
# newest upstream: it takes the ffmpeg.exe of one of our own releases (which
# GitHub keeps for good) and checks its sha256 against FFMPEG_SHA256 below.
#
# To update ffmpeg on purpose: run with --latest, test that build, put it in a
# release, then set FFMPEG_RELEASE to that release and FFMPEG_SHA256 to the
# printed hash. Needs the gh CLI and GH_TOKEN.
set -euo pipefail

# ffmpeg n9.0.2-47-gb204e94e90-20261105 (BtbN win64 LGPL), as shipped in v2.15.0.
FFMPEG_RELEASE=v2.15.0
FFMPEG_SHA256=a502236803095e00ca2b6febf4617c10018198bbc52bb29401653859d000fe62

REPO=gdrretorta/futsal-cam-recorder
ZIP=futsal-cam-recorder-win-x64.zip

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

if [[ "${1:-}" == "--check" ]]; then
  gh release view "$FFMPEG_RELEASE" --repo "$REPO" --json assets --jq '.assets[].name' | grep -Fxq "$ZIP" \
    || { echo "$ZIP missing from $REPO $FFMPEG_RELEASE" >&2; exit 1; }
  echo "ffmpeg from $REPO $FFMPEG_RELEASE"
  exit 0
fi

if [[ "${1:-}" == "--latest" ]]; then
  out=${2:?usage: fetch-ffmpeg.sh --latest <out-dir>}
  UP=BtbN/FFmpeg-Builds
  assets=$(gh release view latest --repo "$UP" --json assets --jq '.assets[].name')
  # Asset names change with each FFmpeg release branch (e.g. ffmpeg-n7.1-latest-win64-lgpl-7.1.zip).
  # LGPL is enough: we only stream-copy, never encode.
  name=$(grep -E '^ffmpeg-n[0-9.]+-latest-win64-lgpl-[0-9.]+\.zip$' <<<"$assets" | sort -V | tail -1 || true)
  [[ -n "$name" ]] || name=$(grep -Fx 'ffmpeg-master-latest-win64-lgpl.zip' <<<"$assets" || true)
  [[ -n "$name" ]] || { echo "no win64-lgpl ffmpeg build found in $UP 'latest'" >&2; exit 1; }
  gh release download latest --repo "$UP" --pattern "$name" --dir "$work"
  unzip -q "$work/$name" -d "$work/x"
  mkdir -p "$out"
  cp "$(find "$work/x" -name ffmpeg.exe | head -1)" "$out/ffmpeg.exe"
  lic=$(find "$work/x" -name LICENSE.txt | head -1)
  [[ -n "$lic" ]] && cp "$lic" "$out/FFMPEG-LICENSE.txt"
  echo "$name"
  sha256sum "$out/ffmpeg.exe"
  exit 0
fi

out=${1:?usage: fetch-ffmpeg.sh <out-dir> | --check | --latest <out-dir>}
gh release download "$FFMPEG_RELEASE" --repo "$REPO" --pattern "$ZIP" --dir "$work"
unzip -q "$work/$ZIP" ffmpeg.exe FFMPEG-LICENSE.txt -d "$work/x"
echo "$FFMPEG_SHA256  $work/x/ffmpeg.exe" | sha256sum -c --quiet - \
  || { echo "ffmpeg.exe from $FFMPEG_RELEASE doesn't match FFMPEG_SHA256" >&2; exit 1; }
mkdir -p "$out"
cp "$work/x/ffmpeg.exe" "$work/x/FFMPEG-LICENSE.txt" "$out/"
ls -la "$out/ffmpeg.exe"
