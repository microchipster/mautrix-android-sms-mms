<!--
SPDX-FileCopyrightText: 2026 Slavi Pantaleev

SPDX-License-Identifier: AGPL-3.0-or-later
-->

# Zero-knowledge file links (CDN protocol)

Status: v1 draft, 2026-10-27.

This document specifies the **zero-knowledge file link** protocol: a client
encrypts a payload locally, uploads the ciphertext, and shares a link whose
fragment carries the decryption key. The server stores and serves opaque bytes
only — it never sees the key, the plaintext, or the original filename/type —
and the browser that opens the link decrypts the payload locally.

The contract is deliberately small (one upload endpoint, one wrapper page, a
handful of ciphertext routes) so third parties can implement compatible
endpoints: a worker in front of object storage, another self-hosted service,
or any HTTPS server that follows the rules below. The protocol is
**generic**: it assumes nothing about the deployment beyond HTTPS. The
reference implementation is the bridge's `/f` service, described for
operators in [cdn-file-service.md](cdn-file-service.md).

The key words **MUST**, **SHOULD** and **MAY** in this document are to be
interpreted as described in RFC 2119.

## 1. Goals and non-goals

### Goals

- Move large attachments out of a message body into an HTTPS link, so clients
  are not bound by carrier or chat message-size limits.
- Zero knowledge: the server stores and serves ciphertext only.
  Confidentiality rests on the link fragment, which HTTP never transmits to
  the server.
- A surface small enough to reimplement (~8 routes, one self-contained HTML
  page family, no accounts for recipients).
- Recipients need no app and no account: any modern browser can open a link,
  decrypt it locally and render common media.
- Safe by default: wrapper pages render only payload types recognized from
  magic bytes, so hostile payloads and manifests cannot execute markup or
  scripts.
- Bounded resources: per-upload limits, per-user quotas and expiry keep one
  user (or one link) from filling the disk forever.

### Non-goals

- **General-purpose storage or backup.** Entries expire or are pruned; links
  are not durable storage and there is no resumable or multipart upload, no
  deduplication, and no sharing between users.
- **A formal standard.** No IETF/W3C document defines this space; this
  document is the contract. See
  [cdn-interop-research.md](cdn-interop-research.md) for the survey of
  third-party services that led to that conclusion.
- **Server-side content services.** No thumbnails, transcoding, previews,
  text search, virus scanning or content moderation: the server cannot process
  what it cannot decrypt.
- **Recipient authentication.** The link is the secret. There are no
  passwords, accounts or access lists.
- **Peer-to-peer transfer.** The server holds the ciphertext, so the sender
  may go offline before the recipient opens the link.

## 2. Roles and trust model

| Role | Responsibilities |
|---|---|
| **Client/uploader** | Generates keys and nonces, encrypts locally, uploads ciphertext, builds links, revokes entries. |
| **Server** | Stores ciphertext plus minimal metadata (id, owner, size, kind, timestamps), enforces expiry and quotas, serves wrapper pages and ciphertext. |
| **Recipient** | Opens the wrapper page, reads the key from `location.hash`, fetches ciphertext and decrypts with WebCrypto. |

Trust properties:

- **Confidentiality**: the AES key exists only in the URL fragment and in the
  client. It is never sent to the server in any request, header or body.
- **Integrity and authenticity**: AES-256-GCM authenticates the ciphertext; a
  wrong key or a corrupted object fails decryption instead of rendering junk.
- **Unguessable ids**: the path id is generated from a CSPRNG; the URL as a
  whole is the capability.
- **Ownership isolation**: upload, list and revoke are scoped to the bearer
  token's owner; foreign ids are indistinguishable from unknown ids (404), so
  the API cannot be used to probe other users' entries.
- **Time bounds**: every entry has an absolute expiry, enforced on read and by
  periodic cleanup.

TLS is required: links contain the only copy of the key and are meant to be
opened over `https://`.

## 3. Link format

```
https://<cdn-base>/f/<id>#<key>
```

- `<cdn-base>`: the server's HTTPS origin (for example `https://cdn.example.com`).
- `<id>`: server-generated, unguessable entry id. Implementations MUST draw it
  from at least 128 bits of CSPRNG entropy; the reference implementation uses
  32 random bytes encoded as unpadded base64url (43 characters from
  `[A-Za-z0-9_-]`). The server MUST validate the id against its own alphabet
  and length bounds before any lookup, so hostile paths are never resolved or
  echoed.
- `<key>`: the 32-byte AES-256 key, unpadded base64url (43 characters), placed
  **after `#`**. This is the fragment and is never transmitted in an HTTP
  request.
- File and album links have the identical shape: `GET /f/<id>` serves the file
  viewer or the album gallery according to the stored kind, so the URL does not
  reveal which one it is.

Client rules:

- Never place the key anywhere else — not in the path, query string, headers
  or request body.
- Never log the full link, and strip the fragment before reposts, thumbnails,
  redirects or analytics. Link unfurlers never receive a fragment, so they
  naturally see only the public page.
- The server cannot reconstruct a link (it never stored the key), so the
  uploader MUST keep the link or re-upload.

## 4. Crypto

### 4.1 Single object

- **Key**: 32 random bytes from a CSPRNG, encoded as unpadded base64url, kept
  in the fragment only.
- **Nonce**: 12 random bytes, fresh for every encryption. A nonce MUST NOT be
  reused under the same key.
- **Cipher**: AES-256-GCM with **no additional authenticated data (AAD)**.
- **Object body**: `nonce (12 bytes) || ciphertext || tag (16 bytes)`. The body
  is exactly 28 bytes larger than the plaintext; clients SHOULD account for
  that against size limits.
- **Decryption**: take the first 12 bytes as the IV and the remainder as
  `ciphertext || tag`; GCM rejects a wrong key or modified bytes.

### 4.2 Albums

An album is one link holding several encrypted items plus one encrypted
manifest, all under a single fragment key `K`. Key reuse across albums is
forbidden.

Subkeys are derived with **HKDF-SHA256** (RFC 5869) with `L = 32` and the
following exact parameters:

```
salt = ASCII bytes of "cdn-album-item"        (14 bytes, no terminator)
info = ASCII bytes of "item:" + decimal(index)   for the item at 0-based index
     = ASCII bytes of "manifest"                 for the manifest object

itemKey(i)  = HKDF-SHA256(ikm = K, salt, info = "item:" + decimal(i), L = 32)
manifestKey = HKDF-SHA256(ikm = K, salt, info = "manifest",            L = 32)
```

With one fragment key and up to 255 items, the on-the-wire info strings are
`item:0`, `item:1`, … and `manifest`. Item names are **not** HKDF input: names
can repeat and are only known after the manifest decrypts, while the index
binds each item to its position in the container and the manifest.

Each item and the manifest are then encrypted exactly as in 4.1, under their
own subkey and with a fresh 12-byte nonce. The browser wrapper MUST reproduce
this derivation with WebCrypto (`crypto.subtle.deriveBits` with
`{name: "HKDF", hash: "SHA-256"}`, `salt` as above), so client and viewer must
implement it identically.

### 4.3 Manifest

The manifest is UTF-8 JSON, encrypted under `manifestKey`, and is never parsed
by the server:

```json
{
  "version": 1,
  "items": [
    {"name": "IMG_0001.jpg", "type": "image/jpeg", "size": 184320},
    {"name": "clip.mp4",     "type": "video/mp4",  "size": 5242880}
  ]
}
```

- `version` MUST be `1`.
- `items` is in display order; item `i` is stored at index `i`.
- `name` is a display/file name (a basename); `type` is the client-declared
  plaintext MIME type; `size` is the plaintext size in bytes (informational;
  the stored ciphertext is 28 bytes larger).
- Readers MUST ignore unknown fields and render `name`/`type` as text only,
  never as markup.
- The declared `type` is advisory for UI grouping. It MUST NOT drive rendering
  decisions — those come from magic-byte sniffing (4.4).

### 4.4 Magic-byte sniffing

The wrapper decides how to present a **decrypted** payload by inspecting its
first bytes; manifest types, filenames and HTTP headers are never trusted.
The reference sniff list is:

| Sniffed type | Leading bytes |
|---|---|
| `image/jpeg` | `FF D8 FF` |
| `image/png` | `89 50 4E 47` |
| `image/gif` | `GIF87a` / `GIF89a` |
| `image/webp` | `RIFF` at 0, `WEBP` at 8 |
| `image/avif` | `ftyp` at 4, `avif`/`avis` at 8 |
| `audio/ogg` | `OggS` |
| `audio/flac` | `fLaC` |
| `audio/wav` | `RIFF` at 0, `WAVE` at 8 |
| `audio/mpeg` | `ID3` or `FF Ex` (MPEG sync) |
| `audio/mp4` | `ftyp` at 4, `M4A ` at 8 |
| `video/webm` | `1A 45 DF A3` |
| `video/mp4` | `ftyp` at 4 (fallback) |
| `application/pdf` | `%PDF-` |

Payloads that sniff to image/audio/video (and PDF for single files) may be
rendered from a `blob:` URL whose MIME type the page itself derived. Anything
else — including HTML, SVG, XML and scripts — MUST be offered as a download
only, never rendered, framed or executed.

## 5. Wire model

All routes live under the server's base URL. JSON is UTF-8. The ciphertext
routes need no credentials: the URL is the secret.

| Route | Auth | Purpose |
|---|---|---|
| `POST /f[?expires=30d]` | bearer | Upload one ciphertext object. Returns `{"id","size","expires_at_ms"}` (201). |
| `POST /f/albums[?expires=30d]` | bearer | Upload one album container (items + manifest). Returns `{"id","size","items","expires_at_ms"}` (201). |
| `GET /f/{id}` | none | Wrapper page: file viewer or album gallery, chosen from the stored kind. |
| `GET /f/{id}/raw` | none | Ciphertext of a single file (404 for album ids, unknown or expired ids). |
| `GET /f/{id}/manifest` | none | Album manifest ciphertext (404 for files, unknown or expired ids). |
| `GET /f/{id}/items/{index}` | none | One album item ciphertext by 0-based index (404 when out of range or for files). |
| `GET /f` | bearer | List the caller's active entries: `{"files":[…],"albums":[…],"cap_bytes","used_bytes"}`. |
| `DELETE /f/{id}` | bearer | Revoke a file or a whole album (204, or 404 for unknown/foreign/expired ids). |

### 5.1 Authentication

- Upload, list and revoke requests MUST carry
  `Authorization: Bearer <token>`. The implementation chooses how tokens are
  issued; the reference deployment maps a paired-device token to its Matrix
  user id and uses that as the ownership/storage key.
- A missing or invalid token MUST yield `401` with the `unauthorized` error
  code (5.7).
- The wrapper page and all ciphertext routes MUST be usable without headers,
  cookies or a session. They MUST NOT require or accept the key.

### 5.2 Upload a single file

```
POST /f[?expires=<duration>]
Authorization: Bearer <token>
Content-Type: application/octet-stream

<nonce || ciphertext || tag>
```

- The body is the raw encrypted object; the server MUST NOT parse or decrypt
  it.
- Success: `201` with `{"id":"<id>","size":<stored bytes>,"expires_at_ms":<unix ms>}`.
  `size` is the stored ciphertext length and is what counts against quotas.
- An empty body is `400 bad_request`; a body above `max_file_bytes` is
  `413 file_too_large`; a body above the user's remaining cap is
  `413 quota_exceeded`.

### 5.3 Upload an album

```
POST /f/albums[?expires=<duration>]
Authorization: Bearer <token>
Content-Type: application/octet-stream
```

One request carries every item and the manifest, so an album is created
atomically (all items stored, or none):

```
magic            8 bytes          ASCII "CDNALB01"
count            1 byte           item count (1..255)
item lengths     count × 8 bytes  uint64 big-endian ciphertext length per item,
                                  in selection order
manifest length  8 bytes          uint64 big-endian manifest ciphertext length
payload          item 0, item 1, …, item count-1, then the manifest
```

Every part is a ciphertext in the single-file format (`nonce || ct || tag`),
encrypted under its own subkey (4.2). The server MUST validate magic, count and
declared sizes before reading the payload, and reject bad magic, a zero item or
manifest length, truncation and trailing bytes with `400 bad_request`. Failure
codes: count above the server cap `album_too_many_items` (400), a per-item body
above `max_file_bytes` `file_too_large` (413), item+manifest ciphertext above
`max_album_bytes` `album_too_large` (413), a manifest above 64 KiB
`album_manifest_too_large` (400), and an album above the user's remaining cap
`quota_exceeded` (413).

Success: `201` with `{"id","size","items","expires_at_ms"}`; `size` is the
stored item + manifest ciphertext, framing excluded.

### 5.4 Wrapper page

`GET /f/{id}` serves a minimal, self-contained HTML page (inline HTML+JS, no
external assets) that:

1. reads the fragment key from `location.hash` and rejects a missing or
   invalid key with a visible message;
2. fetches the ciphertext with `fetch(url, {cache: "no-store"})`;
3. decrypts locally (WebCrypto AES-GCM, plus HKDF for an album manifest and
   items);
4. renders only sniffed image/audio/video payloads (plus sniffed PDF for a
   single file) from a `blob:` URL created by the page itself.

An album page also renders the manifest's names/types/sizes as text
(`textContent` only, never markup) and offers other decrypted payloads as
download cards behind a user gesture. A failing item MUST fail only its own
cell, not the whole grid.

Response headers are part of the contract — see section 7.

### 5.5 Ciphertext routes

`GET /f/{id}/raw`, `GET /f/{id}/manifest` and `GET /f/{id}/items/{index}`
serve stored objects byte-for-byte. Rules:

- Unknown, expired or foreign ids MUST return `404`, exactly like an id that
  never existed.
- `raw` MUST return `404` for album ids; `manifest` and `items` MUST return
  `404` for file ids.
- `index` MUST be the canonical decimal spelling of a 0-based position
  (digits only, no leading zeros, within the stored item count). Anything
  else, including out-of-range positions, MUST return `404`.
- Content headers are specified in section 7. The server MAY send
  `Content-Length`; there is no required range-request support, so clients
  fetch whole objects.

### 5.6 List, revoke and stats

**List** — `GET /f` with the bearer token. Success: `200` with

```json
{
  "files":  [{"id": "…", "size": 1234, "created_at": "…", "expires_at": "…"}, "…"],
  "albums": [{"id": "…", "size": 5678, "item_count": 3, "created_at": "…", "expires_at": "…"}, "…"],
  "cap_bytes": 10737418240,
  "used_bytes": 6912
}
```

- Files and albums are listed separately, newest first; expired entries are
  excluded.
- `used_bytes` covers both kinds (album size = items + manifest) and matches
  the per-user quota accounting.
- `cap_bytes` is the configured per-user cap.
- `created_at`/`expires_at` SHOULD be RFC 3339 timestamps; implementations MAY
  add fields, and clients MUST ignore unknown ones.

**Revoke** — `DELETE /f/{id}` with the bearer token deletes one of the
caller's entries: `204` on success, `404` for unknown, foreign or expired ids.
Revoking an album removes the manifest and every item together.

**Stats** is an operational surface, not a wire route: implementations SHOULD
expose aggregate numbers to operators (active files/albums, bytes, owners,
expired entries awaiting cleanup, last cleanup result) and per-user usage
(`used_bytes`/`cap_bytes` from list) to clients. The reference implementation
exposes them through a management-room command.

### 5.7 Errors

API errors use a JSON envelope:

```json
{"error": "<code>", "message": "human-readable detail"}
```

| HTTP status | Code | Meaning |
|---|---|---|
| 401 | `unauthorized` | Missing or invalid bearer token. |
| 400 | `bad_request` | Malformed request, body, duration or container. |
| 400 | `album_too_many_items` | Album count above the configured item cap. |
| 400 | `album_manifest_too_large` | Manifest ciphertext above 64 KiB. |
| 413 | `file_too_large` | One ciphertext body above `max_file_bytes`. |
| 413 | `album_too_large` | Album item+manifest ciphertext above `max_album_bytes`. |
| 413 | `quota_exceeded` | Upload above the user's per-user cap. |
| 500 | `internal` | Server-side failure. |

`404` responses for unknown/expired/foreign entries are plain (the reference
sends `text/plain`); clients MUST rely on the status code, not on an error
envelope, for them.

## 6. Expiry, quotas and lifecycle

### 6.1 Expiry

- `POST /f` and `POST /f/albums` accept an optional `?expires=<duration>`
  query parameter. The value is a duration string such as `12h`, `7d` or
  `30d` (whole days or hour/minute/second units); it is trimmed and
  case-insensitive.
- A missing value means the server's `default_expiry` (reference default
  **30 days**).
- An invalid, zero or negative value is `400 bad_request`. A value above the
  server's `max_expiry` is also `400`; the maximum is hard-capped at **365
  days** and MUST NOT be configurable above that.
- Expiry is absolute from upload time (`expires_at_ms` in upload responses is
  Unix milliseconds, UTC).
- Expiry is enforced on **every read**: an expired id behaves exactly like an
  unknown one (`404`) even while its bytes are still on disk.
- A periodic cleanup removes expired entries and interrupted uploads (the
  reference runs hourly, with a short initial delay). Bytes may therefore
  linger after the link dies; the link never revives.

### 6.2 Quotas and FIFO pruning

- Each owner has a byte cap (`per_user_cap_bytes`, reference default 10 GiB)
  measured in stored ciphertext. Album size is items + manifest; framing is
  not counted.
- An upload that alone exceeds the cap is rejected with
  `413 quota_exceeded`.
- Otherwise, after an accepted upload the server prunes the owner's **oldest
  entries first (FIFO)** until the total fits the cap. The just-uploaded entry
  is never pruned by its own upload. An album is pruned as a whole: manifest
  and all items disappear together.
- Pruning is silent; clients SHOULD surface remaining quota from
  `used_bytes`/`cap_bytes` so users understand when old links disappear.

### 6.3 Revocation and cleanup

- `DELETE /f/{id}` immediately removes metadata and payload; the link dies at
  once.
- Cleanup also removes orphaned objects or metadata left by interrupted
  uploads, incomplete albums, and stale temporary staging files.
- Implementations SHOULD publish entries atomically (staging + rename, or an
  all-or-nothing album commit) so readers never see a partial entry.

## 7. Wrapper-page discipline

This section is normative for any implementation that serves links to
browsers. The wrapper page is the weakest point of the design, so its headers
and rendering rules are part of the contract.

### 7.1 Response headers

Wrapper page (`GET /f/{id}`):

- `Content-Type: text/html; charset=utf-8`
- `X-Content-Type-Options: nosniff`
- `Content-Security-Policy`: the reference policy is

  ```
  default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src blob:; media-src blob:; object-src blob:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; sandbox allow-scripts
  ```

  The album gallery appends ` allow-downloads` (for its download cards). The
  page needs `script-src`/`style-src` inline because it ships as one
  self-contained file; everything else is locked to `'none'` or `blob:`.
- `X-Frame-Options: DENY`
- `Referrer-Policy: no-referrer`
- `Cache-Control: no-store`

Ciphertext routes (`/raw`, `/manifest`, `/items/{index}`):

- `Content-Type: application/octet-stream` with
  `Content-Disposition: attachment` and `X-Content-Type-Options: nosniff`, so
  nothing ever renders the raw bytes inline.
- `Cache-Control: private` (the reference sends `private, max-age=3600`).
- `Access-Control-Allow-Origin: *`. The wrapper runs in a sandboxed document
  with an opaque origin, so its ciphertext fetches are cross-origin; the URL
  is the secret and the bytes are undecryptable without the fragment key, so
  the wildcard is safe and keeps the sandbox strict.

### 7.2 Rules

- The server MUST NOT receive or store the key, and MUST NOT log or echo more
  of the request than it needs (the id only).
- The wrapper MUST be self-contained: no external scripts, styles, fonts or
  images.
- Rendered payloads MUST come from `blob:` URLs created from the decrypted
  bytes and a MIME type the page itself sniffed (4.4).
- Manifest strings and any other client-supplied text MUST be written with
  `textContent` (or equivalent), never as HTML.
- Downloads MUST be user-gesture driven, use `blob:` URLs with
  `application/octet-stream`, and carry `rel="noopener"`.
- HTML, SVG, XML and scripts MUST never be rendered, framed or executed,
  regardless of what a manifest claims.
- Error and fail states MUST keep the page inert: they replace text, they do
  not navigate.

## 8. Server configuration keys

The normative runtime knobs an implementation needs, with the reference
defaults:

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Master switch; no service without it. |
| `domain` | — | Public hostname the service is served on (bare hostname, no scheme or path), e.g. `cdn.example.com`. Required when enabled. |
| `data_dir` | — | Directory for ciphertext and metadata. Required when enabled. |
| `max_file_bytes` | 512 MiB | One ciphertext upload, and one album item. |
| `max_album_items` | 20 | Items per album; the wire format caps this at 255. |
| `max_album_bytes` | 500 MiB | One album's item + manifest ciphertext. |
| `per_user_cap_bytes` | 10 GiB | Stored ciphertext per owner (files and albums). |
| `default_expiry` | 30d | Lifetime used when an upload omits `?expires`. |
| `max_expiry` | 365d | Longest requestable lifetime; hard-capped at 365d. |
| `gc_interval` | 1h | How often expired/orphaned entries are removed. |

Validation: enabling the service without a domain or data directory,
non-positive size caps, `max_album_items` above 255, `max_expiry` above 365d,
or `default_expiry` above `max_expiry` MUST stop the service with an
actionable error. A disabled service is always valid.

## 9. Compatibility notes

- **Base URL is configuration, not a constant.** The link format is relative
  to `<cdn-base>`, so any conforming endpoint can host it. Clients SHOULD
  allow a user- or operator-supplied override (an advanced
  "Custom CDN base URL" setting, or discovery data from the server) instead
  of hard-coding one host; when set it overrides discovery, when cleared
  discovery wins. Validate `https://` origins and allow `http://` only for
  local testing.
- **Optional upload credentials.** A custom endpoint may require a different
  bearer token than the one used for the primary deployment; clients SHOULD
  support an optional token field next to a custom base URL, and SHOULD
  provide a connection test (e.g. `GET /f` with that token). The setting
  should be inert while URL uploads are disabled entirely.
- **Any conforming endpoint works.** Because the client only needs the
  endpoints in section 5, the crypto in section 4 and the wrapper discipline
  in section 7, the same client interoperates with a worker + object store, a
  self-hosted server, or another deployment — no per-vendor adapter, no
  protocol negotiation. Switching bases does not affect links already shared:
  they keep pointing at the server that stored them.
- **Albums are an extension.** A server MAY implement only the single-file
  subset (`/f`, `/f/{id}`, `/f/{id}/raw`); such links still work for every
  conforming client that only sends single files. Album-capable clients SHOULD
  treat an album upload failure as a user-visible error rather than silently
  losing items.
- **Recipients need the wrapper page.** Uploading ciphertext to plain object
  storage and appending `#<key>` does not make a working link: without a
  conforming viewer the recipient has undecryptable bytes. The wrapper page is
  part of the protocol, not an accessory.
- **Conformance.** In the reference repository,
  [`scripts/validate/t20_cdn.sh`](../scripts/validate/t20_cdn.sh) runs the
  live single-file contract against any base URL
  (`CDN_BASE_URL=… ./scripts/validate/t20_cdn.sh`), and
  `go test ./... -run Cdn -count=1` covers the same contract plus album
  framing, HKDF derivation and wrapper headers in-process. Third-party
  implementations can reuse the shell suite as a starting conformance check.

## See also

- [cdn-file-service.md](cdn-file-service.md) — the reference deployment:
  storage layout, quotas, management commands and playbook wiring.
- [cdn-interop-research.md](cdn-interop-research.md) — why there is no
  third-party standard to adopt, and the comparison of link-based file
  services.
