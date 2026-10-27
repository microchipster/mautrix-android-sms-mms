<!--
SPDX-FileCopyrightText: 2026 Slavi Pantaleev

SPDX-License-Identifier: AGPL-3.0-or-later
-->

# Validation fixtures

| File | Purpose |
| --- | --- |
| `cat.gif` | Tiny animated cat used by the attachment acceptance tests (`t05_attachments.sh`): pushed to the phone, sent as an MMS and compared byte-exact by SHA-256. |
| `make-cat-gif.sh` | Generator for `cat.gif` (ImageMagick drawing primitives). |

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
