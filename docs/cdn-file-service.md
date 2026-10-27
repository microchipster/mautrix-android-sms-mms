<!--
SPDX-FileCopyrightText: 2026 Slavi Pantaleev

SPDX-License-Identifier: AGPL-3.0-or-later
-->

# Bridge-hosted encrypted file service ("CDN")

The bridge can host large encrypted files as links instead of pushing them
through MMS. The app encrypts a file locally (AES-256-GCM), uploads the
ciphertext, and builds a link such as:

```
https://cdn.example.com/f/<id>#<key>
```

The `<id>` is unguessable and lives in the path; the `<key>` lives only in
the URL fragment (`#…`) and is never sent to the server. Opening the link
serves a minimal wrapper page that fetches the ciphertext, decrypts it in the
browser with WebCrypto, and renders the payload from a blob URL. The service
exists to dodge carrier MMS limits, not to be bulk storage.

**Albums** extend the same service: one link holds several encrypted items
plus a small encrypted manifest (item names/types/sizes), and the link serves
a gallery page that decrypts and renders each item client-side. The link
shape is identical, so the URL does not reveal whether it points at a file or
an album.

The feature is **fully configuration-gated**: without `cdn.enabled` and a
`cdn.domain` there is no service (no listener routes, no management command
backing, nothing).

## Security model

- **Zero-knowledge uploads**: the bridge stores and serves ciphertext only.
  It never sees the AES key, the plaintext, or the original filename/type.
- **URL is the secret**: `GET /f/<id>` and the ciphertext routes need no
  headers and no cookies. Whoever has the full link has the file (or the whole
  album); there is no second factor, so treat links like passwords.
- **No payload rendering in the app's domain**: only sniffed image, audio,
  video and PDF types are rendered on the file wrapper page, from a `blob:`
  URL whose MIME type the page itself derived from magic bytes. The album
  gallery renders sniffed image/audio/video payloads the same way and offers
  everything else as a download card. HTML, SVG and scripts are never
  rendered. Both pages set `X-Content-Type-Options: nosniff` and a strict CSP
  with `sandbox allow-scripts` (`default-src 'none'`, `connect-src 'self'`,
  blob-only media/object sources, no forms, no framing); the album page adds
  `allow-downloads` for its download cards.
- **Ciphertext downloads** (`/raw`, `/manifest`, `/items/<index>`) are served
  as `application/octet-stream` with `Content-Disposition: attachment`,
  `X-Content-Type-Options: nosniff` and `Cache-Control: private`, so nothing
  ever renders the raw bytes inline. Because the wrapper pages are sandboxed
  (opaque origin), they also send `Access-Control-Allow-Origin: *`; that is
  safe since the URL is the secret and the bytes are undecryptable without the
  fragment key.
- **Album subkeys**: an album has one fragment key too. The app derives a
  distinct 32-byte subkey per item with HKDF-SHA256, so the manifest and every
  item are encrypted under their own key. The manifest (item names/types/
  sizes) is encrypted as well; the bridge never sees names, types, or any key.
- **Album gallery**: the album wrapper uses the file wrapper's CSP plus
  `allow-downloads` for the "other types" cards. It fetches items one by one,
  renders a payload only when its magic bytes sniff to image/audio/video, and
  writes manifest strings with `textContent` only, so a hostile manifest
  cannot inject markup, scripts or external requests. Downloads are
  `application/octet-stream` blobs behind a user gesture.
- **Expiry**: links die after the upload's expiry (default 1 month,
  configurable per upload, hard-rejected above 1 year). The service treats
  expired files as gone even before the periodic cleanup removes them.
- **Quotas**: each user has a byte cap; uploads beyond it prune the oldest
  files (FIFO) so the cap cannot fill up the disk. Albums count against the
  same cap and are pruned whole.
- **Own files only**: list and revoke are scoped to the uploader's Matrix
  user. Foreign ids are indistinguishable from unknown ids (404), so the API
  cannot probe other users' files. The bridge cannot reconstruct links — the
  keys are never stored.

## Storage layout

Everything lives under `cdn.data_dir` (the playbook points it at
`/data/cdn`, on the existing bridge data volume):

```
<data_dir>/objects/<id>            ciphertext (12-byte GCM nonce + ciphertext)
<data_dir>/objects/<id>.meta.json  metadata: id, owner, size, created_at, expires_at
```

An album uses the same id space (an id is either a single object or a
directory, never both) and the same sidecar, with `kind: "album"`:

```
<data_dir>/objects/<id>                 album directory
<data_dir>/objects/<id>.meta.json       metadata: id, owner, kind=album, item_count,
                                        size (items + manifest), created_at, expires_at
<data_dir>/objects/<id>/manifest        encrypted manifest object
<data_dir>/objects/<id>/items/<index>   encrypted item ciphertext, 0-based index
```

Files are written with `os.CreateTemp` + rename (no fixed shared temp paths),
metadata sidecars are written the same way, and a crash between the two steps
leaves an orphan that the cleanup removes. Album uploads are staged in a
`.tmp-album-*` directory inside the same directory and renamed into place in
one step; interrupted stagings are removed by the cleanup once stale.

## Albums

An album is one link holding up to `cdn.max_album_items` encrypted items plus
a small encrypted manifest. The link shape does not change:

```
https://cdn.example.com/f/<id>#<key>
```

The app encrypts every item locally, builds the manifest, and uploads both in
one request; the bridge stores ciphertext only. Opening the link serves the
gallery page instead of the single-file viewer — `GET /f/<id>` picks the page
from the stored `kind`, so there is no separate album URL shape.

### Limits

- At most `cdn.max_album_items` items (default 20). The container's count
  byte limits the wire format to 255, which is also the hard maximum of the
  config key.
- At most `cdn.max_album_bytes` of item + manifest ciphertext (default
  500 MiB). The container framing is not counted.
- At most 64 KiB of manifest ciphertext (`album_manifest_too_large`
  otherwise): the manifest is metadata, not a second item.
- Each item is additionally capped by `cdn.max_file_bytes`, exactly like a
  single file upload.
- Albums count against `cdn.per_user_cap_bytes` and share the FIFO pruning:
  the oldest entry (file or album) goes first, and a pruned album is removed
  whole, manifest and all items.
- Mixed media is allowed; the manifest order is the display order.

### Upload wire format

One `POST /f/albums` body carries everything, so an album is created
atomically (all items are stored, or none):

```
magic            8 bytes          ASCII "CDNALB01"
count            1 byte           item count (1..255)
item lengths     count × 8 bytes  uint64 big-endian ciphertext length per item, selection order
manifest length  8 bytes          uint64 big-endian manifest ciphertext length
payload          item 0, item 1, …, item count-1, then the manifest
```

Every part is a ciphertext in the single-file format: 12-byte GCM nonce,
ciphertext and tag. The bridge validates the declared count and sizes before
reading any payload, and rejects bad magic, a zero item/manifest length,
truncation and trailing bytes with `bad_request` (400); the count cap is
`album_too_many_items` (400), the album size cap `album_too_large` (413), a
per-item size cap `file_too_large` (413), and a manifest above 64 KiB
`album_manifest_too_large` (400).

### Manifest

The manifest is UTF-8 JSON, encrypted under its own subkey, and it is never
parsed by the bridge:

```json
{
  "version": 1,
  "items": [
    {"name": "IMG_0001.jpg", "type": "image/jpeg", "size": 184320},
    {"name": "clip.mp4",     "type": "video/mp4",  "size": 5242880}
  ]
}
```

- `version` must be `1`.
- `items` is in display order; item `i` is stored at index `i`.
- `name` is a display/file name (the app should send a basename), `type` is
  the plaintext MIME type, `size` is the plaintext size in bytes
  (informational; the ciphertext is 28 bytes larger).
- Readers must ignore unknown fields and render `name`/`type` as text only,
  never as markup.

### Key derivation (exact)

The link fragment holds one 32-byte fragment key `K` (unpadded base64url).
Never reuse a fragment key across albums, and never send it to the bridge.
Subkeys are derived with HKDF-SHA256 (RFC 5869) with `L = 32`:

```
salt = ASCII bytes of "cdn-album-item"            (14 bytes, no terminator)
info = ASCII bytes of "item:" + decimal(index)    for the item at 0-based position index
     = ASCII bytes of "manifest"                   for the manifest object

itemKey(i)  = HKDF-SHA256(ikm = K, salt, info = "item:" + decimal(i), 32)
manifestKey = HKDF-SHA256(ikm = K, salt, info = "manifest", 32)
```

On the wire the info strings are `item:0`, `item:1`, … and `manifest`. Item
names are **not** used as HKDF input: names can repeat and are only known
after the manifest decrypts, while the index binds every item to its
position. Each item is then AES-256-GCM encrypted under `itemKey(i)` with a
fresh random 12-byte nonce and no AAD, exactly like a single file; the
manifest is AES-256-GCM under `manifestKey`.

The gallery page derives the same subkeys in the browser with WebCrypto
(`crypto.subtle.deriveBits` with `{name: "HKDF", hash: "SHA-256"}`), so the
app and the wrapper must implement this derivation identically.

### Gallery page

The album link serves a minimal, self-contained gallery (inline HTML+JS, no
external assets) with the file wrapper's CSP/nosniff discipline plus
`allow-downloads`:

- the manifest is fetched once (`GET /f/<id>/manifest`) and decrypted with
  `manifestKey`; it supplies display text only;
- image cells fetch and decrypt their item and show it as a grid thumbnail
  (off-screen cells load when scrolled into view);
- audio/video cells load and decrypt on demand and become blob players;
- every other type becomes a download card: the plaintext is decrypted in the
  browser and offered as an `application/octet-stream` blob download;
- a failing item shows an error state in its own cell (and a retry/download
  option) without breaking the rest of the grid;
- payloads render only when their magic bytes sniff to image/audio/video, so
  the manifest's `type` field can never make the browser execute or render
  HTML/SVG/JS.

## Configuration

All keys live under `cdn` in the bridge's network config
(`simple-config.yaml`):

```yaml
network:
  cdn:
    # Master switch. Off by default; a missing section means "no service".
    enabled: true

    # Public hostname the reverse proxy serves the service on. Required when
    # enabled. Convention: cdn.<homeserver-domain>.
    domain: cdn.example.com

    # Ciphertext + metadata directory (inside the container).
    data_dir: /data/cdn

    # One encrypted upload in bytes (default 512 MiB).
    max_file_bytes: 536870912

    # One album: item count (default 20, maximum 255) and total item+manifest
    # ciphertext in bytes (default 500 MiB). Albums also count against
    # per_user_cap_bytes.
    max_album_items: 20
    max_album_bytes: 524288000

    # Stored ciphertext per user in bytes (default 10 GiB). Oldest files are
    # pruned first once the cap is exceeded.
    per_user_cap_bytes: 10737418240

    # Default link lifetime (default 30d) and the longest one an upload may
    # request (hard-capped at 365d).
    default_expiry: 30d
    max_expiry: 365d

    # How often expired entries are removed (default 1h).
    gc_interval: 1h
```

Validation runs at startup: enabling the feature without a domain/data
directory, non-positive size caps, an album item cap above 255, an expiry
above 365d, a default expiry above the maximum, or a domain containing a
scheme/path stops the bridge with an actionable error instead of starting a
broken service. A disabled `cdn` section is always valid. The album caps are
ordinary `cdn` keys: the playbook's `simple-config.yaml` template needs
matching role variables to override the bridge defaults (follow-up on the
playbook side; the defaults apply until then).

## HTTP API

| Route | Auth | Purpose |
|---|---|---|
| `POST /f[?expires=30d]` | bearer | Upload one ciphertext body. Returns `{"id","size","expires_at_ms"}` (201). |
| `POST /f/albums[?expires=30d]` | bearer | Upload one album container (items + manifest). Returns `{"id","size","items","expires_at_ms"}` (201). |
| `GET /f/{id}` | none | Wrapper page: the file viewer or the album gallery, chosen from the stored kind. |
| `GET /f/{id}/raw` | none | Ciphertext of a single file, `application/octet-stream` (404 for album ids). |
| `GET /f/{id}/manifest` | none | Album manifest ciphertext (404 for files, unknown or expired ids). |
| `GET /f/{id}/items/{index}` | none | One album item ciphertext by 0-based index (404 out of range, non-canonical, or for files). |
| `GET /f` | bearer | List your active entries: `{"files":[…],"albums":[…],"cap_bytes","used_bytes"}`. `used_bytes` covers both kinds. |
| `DELETE /f/{id}` | bearer | Revoke a file or a whole album (204, or 404 for unknown/foreign ids). |

Upload details:

- Body is the raw ciphertext (`application/octet-stream`); nothing is parsed
  for files. Album bodies are the framed container above: the bridge reads the
  item count and declared sizes, streams each part into the album directory
  and never decrypts anything.
- `expires` accepts duration strings (`12h`, `7d`, `30d`) on both endpoints.
  Missing means the configured default; invalid, zero, or above `max_expiry`
  is a 400.
- Errors use a JSON envelope: `{"error":"<code>","message":"…"}` with codes
  `unauthorized` (401), `bad_request` (400), `album_too_many_items`,
  `album_manifest_too_large` (400), `file_too_large`, `album_too_large` and
  `quota_exceeded` (413), `internal` (500).

## Management-room commands

```
!sms cdn list                  # your active links and albums: id, size, expiry
!sms cdn revoke <id>           # kill one of your links (a file or a whole album)
!sms cdn stats                 # numbers + effective limits (global for admins, yours otherwise)
!sms cdn gc                    # run expiry cleanup now (admins only)
```

Albums appear in the same list (`album: N item(s)`) and count toward the same
usage numbers; revoking an album deletes the manifest and every item.

The implementation ships as `NewCdnCommand(service)` in
`connector/cdn.go`; registering it is a one-liner (below). Commands are
backed by the `CdnService` methods `List`, `Revoke`, `Stats` and `RunGC`, so
the same surface is available to any future admin endpoint.

## Playbook wiring (matrix-docker-ansible-deploy)

The service reuses the bridge's existing pairing listener. This is the
**leanest mounting choice**: no second listener, no extra container port, no
new systemd unit. Traefik distinguishes the CDN hostname from the homeserver
hostname with a `Host()` router pointing at the same container port
(`29322`). The bridge only starts that listener when pairing or the file
service is enabled (see the patch plan).

### Role variables

Added to
`roles/custom/matrix-bridge-mautrix-android-sms-mms/defaults/main.yml`:

```yaml
# Bridge-hosted encrypted file service ("CDN", see the project's docs/cdn-file-service.md).
# Files are encrypted in the app; the bridge stores ciphertext only. The feature stays off
# until cdn_enabled is true and DNS + a reverse-proxy route exist for cdn_domain.
matrix_bridge_mautrix_android_sms_mms_cdn_enabled: false
matrix_bridge_mautrix_android_sms_mms_cdn_domain: "cdn.{{ matrix_bridge_mautrix_android_sms_mms_homeserver_domain }}"
matrix_bridge_mautrix_android_sms_mms_cdn_data_dir: "/data/cdn"
matrix_bridge_mautrix_android_sms_mms_cdn_max_file_bytes: 536870912
matrix_bridge_mautrix_android_sms_mms_cdn_per_user_cap_bytes: 10737418240
matrix_bridge_mautrix_android_sms_mms_cdn_default_expiry: "30d"
matrix_bridge_mautrix_android_sms_mms_cdn_max_expiry: "365d"
matrix_bridge_mautrix_android_sms_mms_cdn_gc_interval: "1h"

matrix_bridge_mautrix_android_sms_mms_container_labels_cdn_enabled: "{{ matrix_bridge_mautrix_android_sms_mms_cdn_enabled }}"
matrix_bridge_mautrix_android_sms_mms_container_labels_cdn_traefik_rule: "Host(`{{ matrix_bridge_mautrix_android_sms_mms_cdn_domain }}`)"
matrix_bridge_mautrix_android_sms_mms_container_labels_cdn_traefik_priority: 0
matrix_bridge_mautrix_android_sms_mms_container_labels_cdn_traefik_entrypoints: "{{ matrix_bridge_mautrix_android_sms_mms_container_labels_traefik_entrypoints }}"
matrix_bridge_mautrix_android_sms_mms_container_labels_cdn_traefik_tls: "{{ matrix_bridge_mautrix_android_sms_mms_container_labels_traefik_entrypoints != 'web' }}"
matrix_bridge_mautrix_android_sms_mms_container_labels_cdn_traefik_tls_certResolver: "{{ matrix_bridge_mautrix_android_sms_mms_container_labels_traefik_tls_certResolver }}"
```

The domain default follows the project convention
`cdn.<homeserver-domain>`; the feature still does nothing until the operator
sets `matrix_bridge_mautrix_android_sms_mms_cdn_enabled: true` and creates
the DNS record.

### Simple-config template

Appended to
`roles/custom/matrix-bridge-mautrix-android-sms-mms/templates/simple-config.yaml.j2`:

```yaml
# Bridge-hosted encrypted file service (see the project's `docs/cdn-file-service.md`).
cdn:
    enabled: {{ matrix_bridge_mautrix_android_sms_mms_cdn_enabled | to_json }}
    domain: {{ matrix_bridge_mautrix_android_sms_mms_cdn_domain | to_json }}
    data_dir: {{ matrix_bridge_mautrix_android_sms_mms_cdn_data_dir | to_json }}
    max_file_bytes: {{ matrix_bridge_mautrix_android_sms_mms_cdn_max_file_bytes | to_json }}
    per_user_cap_bytes: {{ matrix_bridge_mautrix_android_sms_mms_cdn_per_user_cap_bytes | to_json }}
    default_expiry: {{ matrix_bridge_mautrix_android_sms_mms_cdn_default_expiry | to_json }}
    max_expiry: {{ matrix_bridge_mautrix_android_sms_mms_cdn_max_expiry | to_json }}
    gc_interval: {{ matrix_bridge_mautrix_android_sms_mms_cdn_gc_interval | to_json }}
```

### Traefik route

Added to `roles/custom/matrix-bridge-mautrix-android-sms-mms/templates/labels.j2`,
mirroring the pairing block:

```jinja2
{% if matrix_bridge_mautrix_android_sms_mms_container_labels_cdn_enabled %}
traefik.http.services.matrix-mautrix-android-sms-mms-cdn.loadbalancer.server.port={{ matrix_bridge_mautrix_android_sms_mms_pairing_port }}
traefik.http.routers.matrix-mautrix-android-sms-mms-cdn.rule={{ matrix_bridge_mautrix_android_sms_mms_container_labels_cdn_traefik_rule }}
traefik.http.routers.matrix-mautrix-android-sms-mms-cdn.service=matrix-mautrix-android-sms-mms-cdn
traefik.http.routers.matrix-mautrix-android-sms-mms-cdn.entrypoints={{ matrix_bridge_mautrix_android_sms_mms_container_labels_cdn_traefik_entrypoints }}
traefik.http.routers.matrix-mautrix-android-sms-mms-cdn.tls={{ matrix_bridge_mautrix_android_sms_mms_container_labels_cdn_traefik_tls | to_json }}
{% if matrix_bridge_mautrix_android_sms_mms_container_labels_cdn_traefik_tls %}
traefik.http.routers.matrix-mautrix-android-sms-mms-cdn.tls.certResolver={{ matrix_bridge_mautrix_android_sms_mms_container_labels_cdn_traefik_tls_certResolver }}
{% endif %}
{% endif %}
```

Notes:

- DNS: create an A/AAAA record for `cdn.<homeserver-domain>` (the operator
  configures DNS, like every other playbook hostname). Make sure the
  certificate setup covers the name: a wildcard certificate for the domain
  covers it; otherwise Traefik's `certResolver` must be able to issue a
  certificate for the exact hostname.
- `group_vars/matrix_servers`: widen the host-port binding condition of
  `matrix_bridge_mautrix_android_sms_mms_container_http_host_bind_ports` from
  `pairing_enabled` to `(pairing_enabled | bool or matrix_bridge_mautrix_android_sms_mms_cdn_enabled | bool)`,
  so non-Traefik deployments expose the shared port when only the file
  service is on.
- No new container port, volume or service is needed: `/data` is already
  mounted read-write and `/f` lives on the pairing port.

## Bridge code wiring patch plan

The service core is `connector/cdn.go` (this change). Wiring it into the
bridge is this exact patch set:

1. `connector/config.go`:
   - Add a field to `SimpleConfig`:
     ```go
     // CDN configures the bridge-hosted encrypted file service.
     CDN CdnConfig `yaml:"cdn,omitempty"`
     ```
   - Add `CDN: DefaultCdnConfig(),` to `NewSimpleConfig()`.
   - Add an accessor (mirrors the other `*Config()` helpers):
     ```go
     // cdnConfig returns the file service configuration (never nil).
     func (c *MyConnector) cdnConfig() CdnConfig {
         if c == nil || c.config == nil {
             return DefaultCdnConfig()
         }
         return c.config.CDN
     }
     ```
2. `connector/my_connector.go`:
   - Add `cdn *CdnService` to `MyConnector`.
   - In `Init`, after the pairing logger is re-attached (and before command
     registration, so the command sees the service):
     ```go
     if cfg := c.cdnConfig(); cfg.Active() {
         service, err := NewCdnService(c.log, cfg, c.pairingServer().cdnAuthFunc(br))
         if err != nil {
             c.log.Error().Err(err).Msg("File service disabled: invalid cdn configuration")
         } else {
             c.cdn = service
         }
     }
     ```
   - In `Init`, register the command next to the others:
     ```go
     proc.AddHandler(NewCdnCommand(c.cdn))
     ```
   - In `Start`: `if c.cdn != nil { c.cdn.StartGC() }`.
   - In `Stop`: `if c.cdn != nil { c.cdn.StopGC() }`.
   - In `StartPairingServer`, start the listener when either feature needs
     it:
     ```go
     if !c.pairingEnabled() && c.cdn == nil {
         c.log.Info().Msg("App pairing API is disabled by configuration")
         return nil
     }
     ```
3. `connector/pairing_server.go`:
   - Mount the file service on the pairing mux:
     ```go
     if s.connector != nil && s.connector.cdn != nil {
         s.connector.cdn.Register(mux)
     }
     ```
   - Add the auth adapter mapping a device token to its Matrix user:
     ```go
     // cdnAuthFunc maps a paired-device bearer token to its Matrix user, the
     // owner key of file-service requests.
     func (s *pairingServer) cdnAuthFunc(br *bridgev2.Bridge) CdnAuthFunc {
         return func(r *http.Request) (string, bool) {
             if br == nil {
                 return "", false
             }
             device, perr := s.authenticate(r)
             if perr != nil {
                 return "", false
             }
             login, err := br.GetExistingUserLoginByID(r.Context(),
                 networkid.UserLoginID(stableLoginID(pairingDeviceIDPrefix+device.deviceID)))
             if err != nil || login == nil || login.User == nil {
                 return "", false
             }
             return string(login.User.MXID), true
         }
     }
     ```
4. `connector/pairing_protocol.go` + `connector/pairing_server.go`:
   - Add `CDNBaseURL string \`json:"cdn_base_url,omitempty"\`` to
     `pairInfoResponse` and `pairCompleteResponse`.
   - Fill it in `handleInfo` and `handlePairComplete` with the public base
     URL when the service is active:
     ```go
     cdnBaseURL := ""
     if s.connector != nil && s.connector.cdn != nil {
         cdnBaseURL = s.connector.cdn.BaseURL()
     }
     ```
5. `connector/commands.go`: append to `managementRoomHelpText()`:
   `- cdn list | revoke <id> | stats | gc`
6. Playbook: the role variables, simple-config block and Traefik router
   above, plus the `group_vars/matrix_servers` condition tweak.
7. Optional follow-up: a short "Large files as links" section in
   `docs/configuring-playbook-bridge-mautrix-android-sms-mms.md`, and a
   pointer from the bridge repo's `README.md` to this page.

## App-side integration contract

- **Discovery**: the app only offers URL uploads when the bridge advertises
  `cdn_base_url` (e.g. `https://cdn.example.com`) in
  `GET /_smsmms/v1/info` and `POST /_smsmms/v1/pair/complete`. No
  `cdn_base_url` means the feature is off.
- **Encryption**:
  - Generate a random 32-byte key (CSPRNG) and a random 12-byte nonce per
    file.
  - Encrypt with AES-256-GCM, no additional authenticated data.
  - Body = `nonce || ciphertext||tag`. Ciphertext is 28 bytes larger than
    the plaintext; account for that against the size limits.
  - Key encoding: unpadded base64url. It never leaves the device except in
    the link fragment.
- **Upload**: `POST <cdn_base_url>/f` with
  `Authorization: Bearer <paired-device token>`,
  `Content-Type: application/octet-stream`, and the ciphertext body.
  Append `?expires=<duration>` when the user picked a lifetime
  (`12h`, `7d`, `30d`, …). Success is 201 with
  `{"id","size","expires_at_ms"}`.
- **Links**: `<cdn_base_url>/f/<id>#<key>` for both files and albums. Never
  log or sync the fragment; strip it before analytics, thumbnails or reposts.
  The bridge cannot regenerate the link, so the app should keep it (or
  re-upload).
- **Albums**:
  - Pick one random 32-byte fragment key per album (CSPRNG); never reuse it.
  - Derive `itemKey(i)` and `manifestKey` exactly as in the key-derivation
    section (HKDF-SHA256, salt `cdn-album-item`, info `item:<i>`/`manifest`).
  - Encrypt each selected item under its index's subkey and the manifest JSON
    under `manifestKey`, then build the container and
    `POST <cdn_base_url>/f/albums` with the same bearer token and optional
    `?expires=`. One request per album, selection order = item order, mixed
    media allowed.
  - Pre-check `max_album_items`, `max_album_bytes` (items + manifest) and
    `max_file_bytes` per item before uploading; the bridge defaults are 20 and
    500 MiB, but the deployment may lower them.
  - Success is 201 with `{"id","size","items","expires_at_ms"}`; `size` is
    the stored ciphertext (items + manifest, framing excluded).
  - Album management uses the same endpoints: `GET /f` returns albums in the
    `albums` array, `DELETE /f/<id>` revokes the whole album.
- **Management from the app**: `GET <cdn_base_url>/f` lists the device
  user's files and albums; `DELETE <cdn_base_url>/f/<id>` revokes one. Both
  use the same bearer token; foreign/unknown ids return 404.
- **App settings** (matching the parked plan):
  | Setting | Effect |
  |---|---|
  | Ask to compress | Ask before downscaling an oversized attachment; compressing avoids a URL upload. |
  | Allow big files as URLs | Enables the URL path; requires `cdn_base_url`. |
  | Expiry default (+ ask each time) | Value sent as `?expires=`; must not exceed the bridge's 365d cap. |
  | Upload size limit | App-side pre-check against `max_file_bytes` (remember the +28 bytes overhead). |
  | Global expiry override | App-wide default applied to every URL upload. |
- The wrapper pages are server-rendered; the app does not need its own
  viewer. Recipients with any browser can open a link (album links get the
  gallery page), and clients that unfurl URLs must not fetch the fragment
  (they never do — fragments are not sent).

## Validation

`scripts/validate/t20_cdn.sh` runs the single-file contract against a live
base URL (see [validation.md](validation.md)): encrypted round trips for
text/binary data, wrapper-page headers, wrong-key failure, 401/404 handling,
expiry, quotas, revoke and cross-user isolation.

`go test ./... -run Cdn -count=1` covers the same HTTP contract in-process,
including the app's AES-256-GCM steps, and the full album contract: container
framing and caps, manifest round-trip, HKDF subkey derivation (cross-checked
against an RFC 5869 reference and the file helpers), partial item fetches,
gallery-page headers/escaping, quota/FIFO accounting, expiry and whole-album
revocation. Live album probes in `t20_cdn.sh` are a follow-up.

## Caveats

- There is no per-file password beyond the fragment key; a leaked link is a
  leaked file until it is revoked or expires.
- An album link exposes every item at once; anyone with the link can fetch
  items one by one for as long as the album lives. Revoking or expiring the
  album kills the manifest and all items together.
- The bridge cannot see or validate album item names/types — they live only
  in the encrypted manifest. A hostile uploader can only affect viewers of
  their own album, and the gallery still renders manifest strings as text and
  payloads as sniffed blobs.
- Expiry is enforced on read and by the hourly cleanup; an expired link
  returns 404 immediately, bytes may linger on disk until the next cleanup.
- The bridge cannot list, restore or rebuild keys, and cannot show working
  URLs in the management room (only ids).
- Quota pruning silently removes the oldest entries of that user; a pruned
  album loses all of its items. The app should show remaining quota
  (`used_bytes`/`cap_bytes`) before uploading.
- `data_dir` is under the bridge data volume, so bridge backups include the
  ciphertext (still useless without the keys).

## Deferred

- App-side uploader, crypto helpers and settings UI.
- App-side albums: multi-select "Send as album" (subject to the size UX) and
  in-thread thumbnail-grid rendering that opens the gallery viewer.
- Live album probes in `scripts/validate/t20_cdn.sh` (the Go contract tests
  cover the framing, crypto and HTTP behavior in-process).
- Playbook role variables and `simple-config.yaml` lines for
  `max_album_items` / `max_album_bytes`; the bridge defaults apply until they
  land.
- Playbook role variables, simple-config template, Traefik router and DNS
  (patch plan above).
- Command help text in `managementRoomHelpText()` and the parent playbook
  docs page.
- Optional: per-file download counters or Prometheus metrics (the management
  `cdn stats` command is the current surface).
