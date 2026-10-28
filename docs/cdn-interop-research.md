<!--
SPDX-FileCopyrightText: 2026 Slavi Pantaleev

SPDX-License-Identifier: AGPL-3.0-or-later
-->

# CDN/file-link interop research

Question: can the app's encrypted file-link feature "comply to any standard" or
mimic a third-party service (Firefox Send lineage, Wormhole, Bitwarden Send, …)
so it could also target a custom or third-party CDN?

**Bottom line**: no standard exists to comply with; key-in-fragment is a
convention, not a spec. No third-party API is worth mimicking today. Keep the
bridge `/f` contract as the canonical surface, add an app-side "Custom CDN base
URL" advanced setting, and treat a Send-fork adapter as the one deferred option
(best candidate, still not worth building now).

## Options

| Service | Key model | API, auth | Expiry, limits | Self-host | Mimic? |
|---|---|---|---|---|---|
| **This bridge's `/f`** (baseline) | AES-256-GCM key, base64url in fragment; unguessable id in path | `POST /f`, `GET/DELETE /f`, `GET /f/<id>` wrapper; Bearer paired-device token | per-upload `?expires` (default 30d, max 365d); 100 MiB/file, 1 GiB/user FIFO | yes (this playbook) | canonical — don't mimic, publish |
| **Send forks** — `send.vis.ee`, [Timvisee/send](https://github.com/timvisee/send) | 16-byte secret in fragment; AES-128-GCM via ECE/HKDF ([encryption.md](https://github.com/timvisee/send/blob/master/docs/encryption.md)) | undocumented HTTP (v2 two-step → v3 → master also WebSocket `/api/ws`, [routes](https://github.com/timvisee/send/blob/master/server/routes/index.js)); anonymous, owner token for management; `ffsend` CLI is the reference client | per-instance: vis.ee 2.5 GiB / 3 d / 10 downloads ([instances](https://github.com/timvisee/send-instances)); server defaults 1 d, 7 d max, 1–100 downloads, 2.5 GiB max ([config.js](https://github.com/timvisee/send/blob/master/server/config.js)) | yes (MPL-2.0, Docker) | **best candidate; feasible, defer** |
| **Wormhole.app** | key in fragment (`/<room-id>#<key>`) | none — closed source; only the crypto lib is open ([security](https://wormhole.app/security)) | 5 GiB / 24 h server-stored; larger files P2P while sender online ([FAQ](https://wormhole.app/faq)) | no | no |
| **Bitwarden Send** | send id + key in fragment (`<host>/#/<id>/<key>`) | documented vault API (`POST /sends`, `POST /sends/file/v2`, `POST /sends/access`, … — see [Vaultwarden routes](https://github.com/dani-garcia/vaultwarden/blob/main/src/api/core/sends.rs)) + `bw send` CLI; OAuth API key, payloads client-encrypted | file 500 MB (100 MB mobile), premium only; ≤30 d ([create-send](https://bitwarden.com/help/create-send/)) | Bitwarden self-host / Vaultwarden | possible but heavy: paid account + their crypto |
| **ToffeeShare / FilePizza** | no key in URL; DTLS transport, optional password | none | no storage (P2P WebRTC), uploader must stay online; no size limit ([ToffeeShare](https://toffeeshare.com/en), [FilePizza](https://github.com/kern/filepizza)) | FilePizza BSD-3; ToffeeShare closed | no — incompatible with async SMS links |
| **PrivateBin** | key in fragment (`?<id>#<key>`) | documented JSON v2: `PUT/POST <url>` with `X-Requested-With: JSONHttpRequest` ([API](https://github.com/PrivateBin/PrivateBin/wiki/API)) | 10 MiB default paste+attachment; operator TTL presets ([configuration](https://github.com/PrivateBin/PrivateBin/wiki/Configuration)) | yes (PHP) | no — paste UX, not file links |
| **0x0.st / temp.sh** (non-E2E) | none — we would upload our ciphertext and append the fragment ourselves | multipart curl; 0x0.st X-Token/del/expires management, client-author rules ([0x0.st](https://0x0.st/)); temp.sh `POST /upload` | 0x0.st 512 MiB, 30 d–1 y by size; temp.sh 4 GB, 3 d ([temp.sh](https://temp.sh/)) | temp.sh trivial; 0x0.st ops-heavy | no — no wrapper page, recipient cannot decrypt |
| **Proton Drive share links** | password/key in fragment, never sent to servers ([security model](https://proton.me/blog/protondrive-security)) | no supported share API; [SDK](https://github.com/ProtonDriveApps/sdk) is preview and "not officially supported for third-party use" | account required | no | no |
| **S3-compatible (R2/B2/MinIO) + tiny worker** | our scheme, fragment key | implement our `/f`, or presign PUT + a viewer route | operator-defined | yes | not mimicry — the recommended way to host a "custom CDN" |

Firefox Send itself was decommissioned by Mozilla in 2020
([blog](https://blog.mozilla.org/en/uncategorized/update-on-firefox-send-and-firefox-notes));
the lineage lives on only as community forks (plus clone-of-the-week sites like
Marsdrop/Filarr/SaferDrop — same model, same lack of API guarantees).

### Is there a standard?

No. No IETF/W3C/ISO document defines the link format or upload API of
encrypted file-sharing services; the Matrix spec has nothing for it either.
Only adjacent standards exist and are already used here: RFC 8188 (Encrypted
Content-Encoding for HTTP — used by Send and Wormhole for streaming), WebCrypto
(used by every browser client), HKDF (RFC 5869) and AES-GCM. Key-in-fragment is
a convergent convention — Send, Wormhole, Bitwarden Send, PrivateBin, Proton
Drive and countless clones all place the secret after `#` because fragments are
never sent to servers — but each service invents its own API, crypto details
and link syntax around it. "Compliance" here can only mean *documenting our own
contract well enough that others can implement it*.

## What "custom CDN link" could mean in the app

1. **Another implementation of our contract (bring-your-own-CDN).** A
   Cloudflare Worker + R2/B2, a second bridge deployment, or any server that
   implements the five `/f` endpoints and the wrapper page. The app only needs
   a way to point at it (today the base URL is discovery-only, deliberately
   never derived by the app — see `CdnDiscovery.kt`).
2. **An adapter to one specific third-party service.** The app talks that
   service's API and crypto, and the link belongs to it (`/download/<id>/#key`,
   `/#/<id>/<key>`, …). Realism varies per row above; only Send is a sane
   target, and even it needs a crypto port (HKDF + ECE), not just HTTP work.
3. **Dumb storage + our own fragment.** Upload ciphertext to any host (S3
   presigned PUT, WebDAV, 0x0.st, …) and append `#key`. Uploading is easy; the
   recipient has no viewer that decrypts, and list/revoke/expiry/quota
   semantics are lost. Only viable if we also ship a viewer page — which is
   exactly what contract option 1 already provides.
4. **The homeserver as CDN.** Upload ciphertext as Matrix media (`mxc://`) and
   share a `matrix.to` link with the key in the fragment. Recipients must be
   Matrix users, links fragment-handling is client-dependent, and it duplicates
   the bridge's own `/f` path. Rejected.
5. **Per-user BYO bucket proxied by the bridge.** Bridge holds the user's
   storage credentials and proxies uploads. Shifts bandwidth/cost off the
   operator, but adds credential custody and a second storage path. Premature.

## Proposed answer

**Keep our tiny contract canonical and call it done for "standards".**

- Publish the `/f` contract as a one-page spec (`docs/cdn-file-service.md`
  already is one, near enough): endpoints, error envelope, link format,
  crypto rules. Optionally add an OpenAPI file and point at
  `scripts/validate/t20_cdn.sh` as the conformance suite. Interop then means
  *others implement our contract*, which is realistic because it is ~5
  endpoints — unlike reimplementing anyone else's service.
- App: add an advanced **"Custom CDN base URL"** setting (plus an optional
  token field). Semantics: when set, it overrides the discovered
  `cdn_base_url`; when cleared, discovery wins. Validate `https://` (allow
  `http://` only for testing), add a "Test connection" button (`GET /f` with
  the token), and make it inert while "Allow big files as URLs" is off. This
  satisfies interpretations 1 and 3's storage story without new protocol code.
- **One adapter target, deferred: Timvisee/Send** (self-hosted instance or a
  public one). Adapter sketch:
  - Upload: `POST <send-base>/api/upload` with `X-File-Metadata` (base64
    metadata) and `Authorization: send-v1 <verifier>`; stream
    `nonce||ciphertext||tag`… but **only after porting Send's crypto**: a
    16-byte secret key, HKDF-SHA256 key derivation, ECE chunked AES-128-GCM,
    encrypted metadata and HMAC request auth. ttl/download-limit travel
    differently per API generation (JSON in v2/v3, WebSocket message on
    master); `ffsend` proves a third-party client is possible.
  - Link: `<send-base>/download/<id>/#<key>`.
  - Auth: none (anonymous upload); the response `owner` token is stored in app
    prefs for revoke (`POST /api/delete/<id>`).
  - Expiry mapping: our `?expires=7d` → Send `ttl` seconds, clamped to the
    instance maximum (default 7 d; vis.ee 3 d); set `downloadLimit` to the
    instance max (100) to approximate "unlimited"; no quota/listing.
  - Verdict: realistic but not worthwhile now — it buys third-party hosting at
    the price of a crypto port, instance-specific limits and a shifting
    internal API. Revisit only if a concrete instance is a hard requirement.
- Rejected outright: Bitwarden/Proton/Wormhole adapters (account/crypto/closed
  API weight), P2P services (sender must stay online; the SMS recipient often
  opens links hours later), non-E2E curl hosts (no viewer for recipients).

If the adapter path is ever taken, first refactor the app's `CdnClient` behind
a small `CdnBackend` interface (upload → id/key/expiry, link builder, revoke)
so `SendBackend` is one implementation rather than a fork of the current path.

## Open questions

1. Is a specific custom/third-party CDN already in mind (e.g. your own Send
   instance, a Worker + R2, another bridge)? Its name decides whether an
   adapter is ever justified.
2. Should "Custom CDN base URL" be user-settable in the app, or
   operator-only (bridge config advertises it via `cdn_base_url`)? The latter
   is simpler and keeps tokens out of user settings.
3. For a custom CDN that is not a paired bridge: which token model — reuse the
   paired-device token, a separate token field, or no auth (URL-as-secret)?
4. Do you want the Send adapter built (HKDF/ECE crypto port + tests), knowing
   instance limits and that public instances may rate-limit or change?
5. Should we publish the contract as an OpenAPI file plus a minimal reference
   implementation (e.g. a Cloudflare Worker) so "standard-compliant CDN"
   becomes an implementable target?
6. Any priority on recipient UX beyond the current wrapper page (e.g. links
   that keep working if the bridge is down — only possible with a third-party
   CDN)?
