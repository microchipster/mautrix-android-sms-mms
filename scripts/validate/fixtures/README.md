<!--
SPDX-FileCopyrightText: 2026 Slavi Pantaleev

SPDX-License-Identifier: AGPL-3.0-or-later
-->

# Validation fixtures

| File | Purpose |
| --- | --- |
| `cat.gif` | Tiny animated cat used by the attachment acceptance tests (`t05_attachments.sh`, `t16_gif_roundtrip.sh`): pushed to the phone, sent as an MMS and compared byte-exact by SHA-256. |
| `make-cat-gif.sh` | Generator for `cat.gif` (ImageMagick drawing primitives). |
| `paw.png` | Tiny second image for the multi-attachment MMS test (`t17_multi_attachment.sh`). |
| `tiny.mp4` | 1 s 96x96 H.264 video for the media-variant test (`t18_media_variants.sh`); also reused by the CDN matrix (`t20_cdn.sh`). |
| `tiny.m4a` | 1 s 440 Hz mono AAC audio for the media-variant test (outbound and inbound). |
| `tiny.ogg` | 1 s 440 Hz mono Opus audio for the media-variant test (inbound alternative). |
| `tiny.png` | Deterministic 64x64 RGB PNG for the CDN round-trip test (`t20_cdn.sh`). |
| `tiny.pdf` | Deterministic one-page PDF for the CDN round-trip/sniff test (`t20_cdn.sh`). |
| `make-media-fixtures.sh` | Generator for `paw.png`, `tiny.mp4`, `tiny.m4a` and `tiny.ogg` (ImageMagick + ffmpeg lavfi). |
| `make-cdn-fixtures.sh` | Generator for `tiny.png` and `tiny.pdf` (python3 stdlib only, deterministic). |

## Provenance

`cat.gif` is generated **in-house** by `make-cat-gif.sh` from ImageMagick drawing
primitives (ellipses, polygons, paths) only. It contains no third-party artwork,
photographs, fonts, brand marks or other copyrighted material; the generator is part of
this repository and carries the project SPDX header. The cat is a generic cartoon drawn
from geometric shapes.

Properties:

- animated GIF, 6 frames, 320x240;
- well under the 100 KB budget the tests assert (and far below any carrier MMS limit);
- deterministic drawing; the encoder may rewrite the GIF bytes on regeneration, so the
  tests hash the *local file on disk*, not a pinned upstream hash.

## Regeneration

```sh
scripts/validate/fixtures/make-cat-gif.sh
# or, into a different path:
scripts/validate/fixtures/make-cat-gif.sh /tmp/cat.gif
```

Requires ImageMagick (`magick` or `convert`); the script exits non-zero if the result
exceeds 100000 bytes. Current size: 32070 bytes (SHA-256
`20a00f4641e1c2451d8a50ceae4406709e1ba9ddf2b23fd4d7a5d17c4cf689f2`).

```sh
scripts/validate/fixtures/make-media-fixtures.sh
# or, into a different directory:
scripts/validate/fixtures/make-media-fixtures.sh /tmp/fixtures
```

Requires ImageMagick and ffmpeg. All four outputs are generated in-house from ImageMagick
drawing primitives and ffmpeg `lavfi` synthetic sources (`color`, `sine`) only — no
third-party artwork, audio, photographs or fonts. Current sizes and hashes (the encoders
may rewrite bytes on regeneration, so the tests hash the local file on disk, not a pinned
hash):

| File | Bytes | SHA-256 |
| --- | --- | --- |
| `paw.png` | 2254 | `ba5b750d3e5582abd67d24024e4c432e4d03b274d2239ad257e3781c30eab391` |
| `tiny.mp4` | 1837 | `7cd5ae03249afb8adc0588b77311577a4dc9146874006ff60eee76fe397f1d71` |
| `tiny.m4a` | 3940 | `7fc4a7ca8f0fe5f7c6be350f1f5bc480942725c180d7e7630b9497a589346163` |
| `tiny.ogg` | 3209 | `b552d94313098de39f4d2cb530645876a230a84d87043714affa2457de265cf5` |

```sh
scripts/validate/fixtures/make-cdn-fixtures.sh
# or, into a different directory:
scripts/validate/fixtures/make-cdn-fixtures.sh /tmp/fixtures
```

Requires python3 only. Both outputs are written from python3 stdlib primitives
(`struct`/`zlib`), so they reproduce byte-for-byte across machines unless Python's
zlib encoder changes — no third-party artwork, text, fonts or metadata. Current sizes
and hashes:

| File | Bytes | SHA-256 |
| --- | --- | --- |
| `tiny.png` | 10362 | `515a9b17edac1e580fbd9f711659cb619b741ce7b5e5ba92d7ead150b004e23b` |
| `tiny.pdf` | 596 | `410e8550fe57d67d7c2d26244743bf066da2e172801815ef195e05049772ffa6` |
