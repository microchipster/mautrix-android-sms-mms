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

The feature is **fully configuration-gated**: without `cdn.enabled` and a
`cdn.domain` there is no service (no listener routes, no management command
backing, nothing).

## Security model

- **Zero-knowledge uploads**: the bridge stores and serves ciphertext only.
  It never sees the AES key, the plaintext, or the original filename/type.
- **URL is the secret**: `GET /f/<id>` and `GET /f/<id>/raw` need no headers
  and no cookies. Whoever has the full link has the file; there is no second
  factor, so treat links like passwords.
- **No payload rendering in the app's domain**: only sniffed image, audio,
  video and PDF types are rendered on the wrapper page, from a `blob:` URL
  whose MIME type the page itself derived from magic bytes. HTML, SVG and
  scripts are never rendered. The wrapper sets
  `X-Content-Type-Options: nosniff` and a strict CSP with `sandbox
  allow-scripts` (`default-src 'none'`, `connect-src 'self'`, blob-only
  media/object sources, no forms, no framing).
- **Ciphertext downloads** are served as `application/octet-stream` with
  `Content-Disposition: attachment`, `X-Content-Type-Options: nosniff` and
  `Cache-Control: private`, so nothing ever renders the raw bytes inline.
  Because the wrapper page is sandboxed (opaque origin), `/raw` also sends
  `Access-Control-Allow-Origin: *`; that is safe since the URL is the secret
  and the bytes are undecryptable without the fragment key.
- **Expiry**: links die after the upload's expiry (default 1 month,
  configurable per upload, hard-rejected above 1 year). The service treats
  expired files as gone even before the periodic cleanup removes them.
- **Quotas**: each user has a byte cap; uploads beyond it prune the oldest
  files (FIFO) so the cap cannot fill up the disk.
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

Files are written with `os.CreateTemp` + rename (no fixed shared temp paths),
metadata sidecars are written the same way, and a crash between the two steps
leaves an orphan that the cleanup removes.

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

    # One encrypted upload in bytes (default 100 MiB).
    max_file_bytes: 104857600

    # Stored ciphertext per user in bytes (default 1 GiB). Oldest files are
    # pruned first once the cap is exceeded.
    per_user_cap_bytes: 1073741824

    # Default link lifetime (default 30d) and the longest one an upload may
    # request (hard-capped at 365d).
    default_expiry: 30d
    max_expiry: 365d

    # How often expired files are removed (default 1h).
    gc_interval: 1h
```

Validation runs at startup: enabling the feature without a domain/data
directory, non-positive size caps, an expiry above 365d, a default expiry
above the maximum, or a domain containing a scheme/path stops the bridge with
an actionable error instead of starting a broken service. A disabled `cdn`
section is always valid.

## HTTP API

| Route | Auth | Purpose |
|---|---|---|
| `POST /f[?expires=30d]` | bearer | Upload one ciphertext body. Returns `{"id","size","expires_at_ms"}` (201). |
| `GET /f/{id}` | none | Wrapper page (paste-link-into-browser endpoint). |
| `GET /f/{id}/raw` | none | Ciphertext, `application/octet-stream`. |
| `GET /f` | bearer | List your active files: `{"files":[…],"cap_bytes","used_bytes"}`. |
| `DELETE /f/{id}` | bearer | Revoke your file (204, or 404 for unknown/foreign ids). |

Upload details:

- Body is the raw ciphertext (`application/octet-stream`); nothing is parsed.
- `expires` accepts duration strings (`12h`, `7d`, `30d`). Missing means the
  configured default; invalid, zero, or above `max_expiry` is a 400.
- Errors use a JSON envelope: `{"error":"<code>","message":"…"}` with codes
  `unauthorized` (401), `bad_request` (400), `file_too_large` and
  `quota_exceeded` (413), `internal` (500).

## Management-room commands

```
!sms cdn list                  # your active links: id, size, expiry
!sms cdn revoke <id>           # kill one of your links
!sms cdn stats                 # global numbers for admins, your usage otherwise
!sms cdn gc                    # run expiry cleanup now (admins only)
```

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
matrix_bridge_mautrix_android_sms_mms_cdn_max_file_bytes: 104857600
matrix_bridge_mautrix_android_sms_mms_cdn_per_user_cap_bytes: 1073741824
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
- **Link**: `<cdn_base_url>/f/<id>#<key>`. Never log or sync the fragment;
  strip it before analytics, thumbnails or reposts. The bridge cannot
  regenerate the link, so the app should keep it (or re-upload).
- **Management from the app**: `GET <cdn_base_url>/f` lists the device
  user's files; `DELETE <cdn_base_url>/f/<id>` revokes one. Both use the
  same bearer token; foreign/unknown ids return 404.
- **App settings** (matching the parked plan):
  | Setting | Effect |
  |---|---|
  | Ask to compress | Ask before downscaling an oversized attachment; compressing avoids a URL upload. |
  | Allow big files as URLs | Enables the URL path; requires `cdn_base_url`. |
  | Expiry default (+ ask each time) | Value sent as `?expires=`; must not exceed the bridge's 365d cap. |
  | Upload size limit | App-side pre-check against `max_file_bytes` (remember the +28 bytes overhead). |
  | Global expiry override | App-wide default applied to every URL upload. |
- The wrapper page is server-rendered; the app does not need its own viewer.
  Recipients with any browser can open the link, and clients that unfurl URLs
  must not fetch the fragment (they never do — fragments are not sent).

## Validation

`scripts/validate/t20_cdn.sh` runs this contract against a live base URL (see
[validation.md](validation.md)): encrypted round trips for text/binary data,
wrapper-page headers, wrong-key failure, 401/404 handling, expiry, quotas,
revoke and cross-user isolation. `go test ./... -run Cdn -count=1` covers the
same HTTP contract in-process, including the app's AES-256-GCM steps.

## Caveats

- There is no per-file password beyond the fragment key; a leaked link is a
  leaked file until it is revoked or expires.
- Expiry is enforced on read and by the hourly cleanup; an expired link
  returns 404 immediately, bytes may linger on disk until the next cleanup.
- The bridge cannot list, restore or rebuild keys, and cannot show working
  URLs in the management room (only ids).
- Quota pruning silently removes the oldest files of that user; the app
  should show remaining quota (`used_bytes`/`cap_bytes`) before uploading.
- `data_dir` is under the bridge data volume, so bridge backups include the
  ciphertext (still useless without the keys).

## Deferred

- App-side uploader, crypto helpers and settings UI.
- Playbook role variables, simple-config template, Traefik router and DNS
  (patch plan above).
- Command help text in `managementRoomHelpText()` and the parent playbook
  docs page.
- Optional: per-file download counters or Prometheus metrics (the management
  `cdn stats` command is the current surface).
