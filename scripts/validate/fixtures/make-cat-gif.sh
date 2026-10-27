#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# make-cat-gif.sh — regenerate fixtures/cat.gif, the tiny animated cat used by
# the attachment tests (t05). Generated in-house from ImageMagick drawing
# primitives; no third-party artwork or fonts are involved.
#
# Usage:
#   scripts/validate/fixtures/make-cat-gif.sh [output.gif]
#
# Requires ImageMagick (`magick` or `convert`). The output is deterministic
# apart from the GIF encoder's own metadata (ImageMagick writes no timestamps
# by default); keep the output under 100 KB — the MMS acceptance tests assert
# that.

set -euo pipefail

OUT="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/cat.gif}"

if command -v magick >/dev/null 2>&1; then
	IM=magick
elif command -v convert >/dev/null 2>&1; then
	IM=convert
else
	echo "ImageMagick (magick or convert) is required" >&2
	exit 1
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-cat-gif.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

# Six frames: ears tilt, pupils track left/right, tail sweeps. Canvas 320x240,
# flat colors, so the GIF stays a few kilobytes.
make_frame() {
	local idx="$1" tail_dx="$2" pupil_dx="$3"
	"$IM" -size 320x240 "xc:#f3e9d2" \
		-stroke none -fill "#e8dcc0" -draw "ellipse 160,215 150,25 0,360" \
		-stroke "#5b3a1a" -strokewidth 4 -fill "#c98a3f" \
		-draw "polygon 88,108 78,46 132,92" \
		-draw "polygon 232,108 242,46 188,92" \
		-draw "ellipse 160,150 82,64 0,360" \
		-stroke none -fill "#c98a3f" \
		-draw "ellipse 110,170 26,18 -20,200" \
		-draw "ellipse 210,170 26,18 160,380" \
		-stroke "#5b3a1a" -strokewidth 3 -fill none \
		-draw "path 'M 160,175 q -14,10 -28,2'" \
		-draw "path 'M 160,175 q 14,10 28,2'" \
		-stroke none -fill "#ffffff" \
		-draw "circle 130,138 130,126" \
		-draw "circle 190,138 190,126" \
		-fill "#2f2f2f" \
		-draw "circle $((130 + pupil_dx)),138 $((130 + pupil_dx)),131" \
		-draw "circle $((190 + pupil_dx)),138 $((190 + pupil_dx)),131" \
		-fill "#e8919b" -draw "circle 160,162 160,157" \
		-stroke "#5b3a1a" -strokewidth 2 \
		-draw "line 100,152 60,146" -draw "line 100,158 58,160" \
		-draw "line 220,152 260,146" -draw "line 220,158 262,160" \
		-stroke "#8a5a2b" -strokewidth 9 -fill none \
		-draw "path 'M 238,196 q 34,$((8 + tail_dx * -1)) 44,$((-14 + tail_dx))'" \
		"$WORK/frame-$idx.png"
}

# Five tail/pupil positions, then the same as the first for a smooth loop.
make_frame 0 0 0
make_frame 1 6 3
make_frame 2 10 4
make_frame 3 6 3
make_frame 4 0 0
make_frame 5 -6 -3

"$IM" -delay 18 -loop 0 "$WORK"/frame-*.png "$OUT"

SIZE="$(stat --format=%s "$OUT")"
printf 'wrote %s (%s bytes)\n' "$OUT" "$SIZE"
if [ "$SIZE" -gt 100000 ]; then
	printf 'error: %s exceeds 100000 bytes\n' "$OUT" >&2
	exit 1
fi
