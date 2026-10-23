# SMS/MMS app pairing protocol (v1)

Canonical wire protocol between the Matrix bridge (`mautrix-android-sms-mms`) and the
Fossify Messages fork (`org.fossify.messages.bridge`). The app polls the bridge over HTTPS
(reverse-proxied); no LAN/port-forwarding assumptions. The adb agent transport
(`android-sms-mms-adb-agent`) is separate and unchanged.

Status: v1 draft, 2026-10-23.

## 1. Actors and trust

- **Bridge**: runs on the Matrix homeserver host. Owns pairing sessions and per-device
  tokens. Exposes the public API under `/_smsmms/v1/` (separate listener, default
  `:29322`, reverse-proxied with TLS).
- **App**: the paired phone. Generates a device keypair, polls for work, pushes events,
  executes SMS/MMS sends with its default-SMS-app permissions.
- **Bot**: `!sms login` in the management room starts pairing; the login flow renders a QR
  code (`display_and_wait` step) whose payload carries homeserver URL, bridge URL and the
  one-time code. A manual path shows the same data as text for users who cannot scan.

Security properties: TLS everywhere; one-time code (8 chars, 10 min TTL, single use,
rate-limited, bound to the MXID that started it); device possession proven by an ECDSA
P-256 signature over a server challenge; per-device bearer tokens (32 random bytes,
SHA-256-hashed at rest) that are revocable (`!sms unpair`); room confirmation message on
success so the user sees what got paired.

## 2. Pairing payload

Both QR and manual paths carry the same payload:

```
smsmms://pair?hs=<urlencoded homeserver_url>&b=<urlencoded bridge_base_url>&code=<CODE>&v=1
```

- `hs`: homeserver base URL the user's account lives on (e.g. `https://matrix.example.com`).
- `b`: bridge public base URL (e.g. `https://matrix.example.com` when reverse-proxied;
  may differ when the bridge is hosted elsewhere).
- `code`: 8 characters, Crockford base32 alphabet (no `I`, `L`, `O`, `U`), case-insensitive.
- Manual entry path: app settings screen accepts `hs`, `b` (optional override) and `code`
  typed by the user.

Discovery (when `b` is absent/broken): app fetches `{hs}/.well-known/matrix/client` and
reads the custom key `chat.example.bridge.sms_mms`:

```json
{"chat.example.bridge.sms_mms": {"url": "https://matrix.example.com", "protocol_version": 1}}
```

The playbook injects this key; a manual override in the app settings always wins.

## 3. Device identity

- Keypair: ECDSA P-256 generated in the Android Keystore (non-exportable private key).
- Public key: SPKI DER, base64url (unpadded) → sent as `pubkey`.
- Device ID: app-generated, stable per install (e.g. `android-<random>`), never the adb
  serial unless the user sets `bridge_device_id` for cross-transport room identity.
- Device signature: `ECDSA-SHA256(signing_input)` where `signing_input` is the UTF-8 string

  ```
  smsmms-pair-v1|<pairing_id>|<server_nonce>|<client_nonce>|<device_id>
  ```

## 4. Endpoints

All JSON; errors use `{"error": "<code>", "message": "<text>"}` with codes
`bad_request`, `unauthorized`, `not_found`, `expired`, `too_many_attempts`,
`unsupported`, `rate_limited`, `internal`.

### `GET /_smsmms/v1/info` (no auth)
```json
{"protocol_version": 1, "bridge_name": "mautrix-android-sms-mms",
 "homeserver_url": "https://matrix.example.com", "pairing_enabled": true}
```

### `POST /_smsmms/v1/pair/start` (no auth)
Request:
```json
{"code": "ABCD1234",
 "hs_url": "https://matrix.example.com",
 "device": {"id": "android-ab12", "name": "Pixel 2 XL", "platform": "android",
            "app_version": "1.10.1-bridge1"},
 "pubkey": "<base64url SPKI DER>",
 "client_nonce": "<base64url 16 bytes>"}
```
Response `200`:
```json
{"pairing_id": "pr_<random>", "server_nonce": "<base64url 16 bytes>", "expires_at_ms": 1792753000000}
```
Errors: `not_found` (bad/used code), `expired`, `too_many_attempts`, `rate_limited`.

### `POST /_smsmms/v1/pair/complete` (no auth, but requires the started session)
Request:
```json
{"pairing_id": "pr_<random>", "signature": "<base64url ECDSA signature>"}
```
Response `200`:
```json
{"device_id": "android-ab12", "token": "<base64url 32 bytes>",
 "user": {"mxid": "@alice:example.com", "displayname": "Alice"},
 "homeserver_url": "https://matrix.example.com",
 "bridge": {"base_url": "https://matrix.example.com", "protocol_version": 1},
 "queue": {"wait_max_ms": 30000}}
```
Side effects: the login appears in the bridge (`LoginMetadata` with device id, pubkey and
token hash), the Matrix login flow completes, the bot posts a confirmation into the room.
`token` is returned exactly once.

### `GET /_smsmms/v1/queue?since=<cursor>&wait_ms=<ms>` (Bearer)
Long-poll for work. `wait_ms` capped at 30000. Response:
```json
{"cursor": 42, "reset": false, "commands": [
  {"seq": 41, "type": "send_sms", "send_id": "…", "sub_id": 1,
   "recipients": ["+15551234567"], "text": "hello"},
  {"seq": 42, "type": "send_mms", "send_id": "…", "sub_id": 1,
   "recipients": ["+15551234567", "+15557654321"], "text": "hi all",
   "attachments": [{"content_type": "image/jpeg", "file_name": "photo.jpg", "base64": "…"}]},
  {"seq": 43, "type": "mark_read", "thread_id": 12, "up_to_ms": 1792753000000}
]}
```
Command types mirror the agent's send API: `send_sms`/`send_mms` (same fields as
`POST /v1/send` in `docs/AGENT_API.md`), `mark_read` (same as `POST /v1/read`), `ping`.
`reset: true` means the cursor is too old — the app must re-send its current state via
`events` and continue from the returned cursor.

### `POST /_smsmms/v1/events` (Bearer)
App → bridge event push; body is the agent `Event` shape plus payloads:
```json
{"device_id": "android-ab12", "cursor": 17, "events": [
  {"seq": 16, "type": "message.new", "message_id": "sms:1:66", "thread_id": 33,
   "sub_id": 1, "timestamp_ms": 1792749554448, "sender": "+15551234567",
   "is_mms": false, "has_attachment": false, "preview": "…", "send_id": "…"}
]}
```
Also `message.updated` (read/seen/status changes), `mms.downloaded`, `thread.updated`,
`send.result` (outcome for a queue command: `{send_id, message_id, thread_id, parts_sent,
pending}` or `{send_id, error: {code, message}}`) and `ack` (a non-send queue command was
handled: `{seq, type: "ack", send_id: "ack-<command_seq>", thread_id, sub_id,
timestamp_ms}` plus an optional `error: {code, message}`). An `ack` removes the matching
queued command (`send_id`, plus the `ack-<seq>` suffix fallback) so it is never redelivered;
unknown event types are stored in the device inbox and ignored. Response:
`{"cursor": 17, "accepted": 3}`.

### `POST /_smsmms/v1/heartbeat` (Bearer)
```json
{"device_id": "android-ab12", "battery_pct": 88, "default_sms_app": true,
 "permissions": {"receive_sms": true, "send_mms": true}, "app_version": "1.10.1-bridge1"}
```
Used for bridge health display and to gate MMS/read features.

## 5. Behaviour rules

- The bridge treats an app-paired login exactly like an adb-agent login at the connector
  layer: same `Message`/`Event` JSON, same portal IDs (`MakePortalID(deviceScope,
  subID, threadID)`), same echo suppression (by `send_id`).
- `send.result` is authoritative for the send: the app must report `pending: true` when the
  provider row has not been resolved yet (bridge then waits for the matching
  `message.new`).
- Queue is per-login (per device); commands are removed once acknowledged by an `events`
  batch that includes their `send.result`/`ack`. Undelivered commands are re-sent after the
  token's device reconnects (bridge keeps them up to 24 h).
- Cursors are monotonic per direction (bridge→app commands, app→bridge events) and survive
  restarts on both sides.
- Timestamps are milliseconds since epoch everywhere (MMS provider seconds are converted at
  the agent/app boundary).
- Attachments are base64 with a hard cap (10485760 bytes encoded per attachment; the bridge
  rejects larger sends with `unsupported`).

## 6. Bridge-side storage

- Pairing sessions: in-memory with TTL (10 min) — bridge restarts invalidate in-flight
  pairings; users just re-run `!sms login`.
- Devices: persisted on the bridgev2 login (`LoginMetadata`: `device_id`, `device_name`,
  `pubkey_spki`, `token_sha256`, `paired_at_ms`, `last_seen_ms`, `transport: "app"`).
  `!sms devices` lists them, `!sms unpair <device_id|all>` revokes (deletes token hash and
  the bridgev2 login/portals as chosen).
- Multiple devices per Matrix account are supported; room/space naming includes a device
  suffix when more than one login exists.

## 7. Open items for v1.1

- Token rotation endpoint; per-request signatures (optional hardening).
- Push notifications (app-initiated long-poll is battery-costly in doze; consider FCM-free
  alternative such as the app keeping the FGS and polling with exponential backoff, plus
  `adb reverse` option when the phone is on USB).
- Bridge URL discovery when the homeserver is on a different host than the bridge
  (the playbook already supports a separate bridge public URL).
