#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# make-media-fixtures.sh — regenerate the extra validation fixtures used by
# the message-type matrix tests:
#
#   paw.png    tiny second image for the multi-attachment MMS test (t17)
#   tiny.mp4   one-second 96x96 H.264 video (t18 inbound video)
#   tiny.m4a   one-second 440 Hz AAC/LC audio (t18 audio both directions)
#   tiny.ogg   one-second 440 Hz Opus audio (t18 inbound audio, alternative)
#
# Everything is generated in-house from ImageMagick drawing primitives and
# ffmpeg synthetic sources (lavfi color/sine) — no third-party artwork, audio
# or fonts. The output is deterministic apart from the encoders' own metadata;
# the tests hash the local files on disk, not pinned upstream hashes.
#
# Usage:
#   scripts/validate/fixtures/make-media-fixtures.sh [output-dir]
#
# Requires ImageMagick (`magick` or `convert`) and ffmpeg.

set -euo pipefail

OUT_DIR="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"
mkdir -p "$OUT_DIR"

if command -v magick >/dev/null 2>&1; then
	IM=magick
elif command -v convert >/dev/null 2>&1; then
	IM=convert
else
	echo "ImageMagick (magick or convert) is required" >&2
	exit 1
fi
command -v ffmpeg >/dev/null 2>&1 || { echo "ffmpeg is required" >&2; exit 1; }

# paw.png — a small flat-color pad print, drawn from ellipses only.
"$IM" -size 96x96 "xc:#f3e9d2" \
	-stroke none -fill "#c98a3f" \
	-draw "ellipse 30,30 16,20 0,360" \
	-draw "ellipse 66,30 16,20 0,360" \
	-draw "ellipse 30,68 16,20 0,360" \
	-draw "ellipse 66,68 16,20 0,360" \
	-draw "ellipse 48,58 20,14 0,360" \
	"$OUT_DIR/paw.png"

# tiny.mp4 — synthetic 1 s 96x96 H.264 video, no audio, faststart.
ffmpeg -hide_banner -loglevel error -y \
	-f lavfi -i "color=c=#336699:s=96x96:d=1:r=10" \
	-an -c:v libx264 -preset veryfast -crf 30 -pix_fmt yuv420p \
	-movflags +faststart -map_metadata -1 \
	"$OUT_DIR/tiny.mp4"

# tiny.m4a — 1 s 440 Hz sine, mono AAC/LC at 24 kbit/s.
ffmpeg -hide_banner -loglevel error -y \
	-f lavfi -i "sine=frequency=440:sample_rate=8000:duration=1" \
	-ac 1 -c:a aac -b:a 24k -map_metadata -1 \
	"$OUT_DIR/tiny.m4a"

# tiny.ogg — 1 s 440 Hz sine, mono Opus at 24 kbit/s.
ffmpeg -hide_banner -loglevel error -y \
	-f lavfi -i "sine=frequency=440:sample_rate=8000:duration=1" \
	-ac 1 -c:a libopus -b:a 24k -map_metadata -1 \
	"$OUT_DIR/tiny.ogg"

for f in paw.png tiny.mp4 tiny.m4a tiny.ogg; do
	size="$(stat --format=%s "$OUT_DIR/$f")"
	sum="$(sha256sum "$OUT_DIR/$f" | cut -d ' ' -f 1)"
	printf 'wrote %s (%s bytes, sha256 %s)\n' "$OUT_DIR/$f" "$size" "$sum"
	if [ "$size" -gt 100000 ]; then
		printf 'error: %s exceeds 100000 bytes\n' "$f" >&2
		exit 1
	fi
done
