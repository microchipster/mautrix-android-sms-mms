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
reads the custom key `chat.<namespace>.bridge.sms_mms`:

```json
{"chat.<namespace>.bridge.sms_mms": {"url": "https://matrix.example.com", "protocol_version": 1}}
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
  {"seq": 43, "type": "mark_read", "thread_id": 12, "up_to_ms": 1792753000000},
  {"seq": 44, "type": "fetch_message", "message_id": "mms:1:534"}
]}
```
Command types mirror the agent's send API: `send_sms`/`send_mms` (same fields as
`POST /v1/send` in `docs/AGENT_API.md`), `mark_read` (same as `POST /v1/read`), `ping`.
`fetch_message` asks the app to push the given message again as a `message.new` event
with its full payload and inline parts (see below); it is sent when an event arrived
without that payload. The app must tolerate the same message being requested more than
once (the bridge retries when an answer does not arrive, for example after a queue reset
swallowed the command).
`merge_threads.result` reports the outcome of a `merge_threads` app request (below):
on success it carries `send_id` (the request's correlation id), the request's
`thread_ids` and `target_thread`, and `room_id` (the Matrix room of the merged target);
on a refusal or failure it carries `error: {code, message}` instead of `room_id`
(`bad_request` for an invalid selection, `internal` when applying a valid one failed).
The app shows the outcome and acknowledges the command like any other.
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
Also `message.updated` (read/seen/status changes), `mms.downloaded`, `thread.updated`
(`thread.updated` may carry `recipients`, the current participant list of the thread; app
transports have no thread list endpoint, so this is how the bridge refreshes a portal's
stored participants — e.g. after a bridge restart — without waiting for a new message).
`merge_threads` asks the bridge to combine the rooms of several phone threads the user
selected in the app's conversation list (the conversation-list merge action):

```json
{"seq": 18, "type": "merge_threads", "send_id": "merge-1761600000000-1",
 "thread_ids": [11, 12], "target_thread": 12}
```

`thread_ids` lists at least two phone threads of this login; `target_thread` names the
thread whose room becomes the merged target and defaults to the first thread (it must be
part of the selection). The bridge resolves each thread to its portal, applies the same
validation and the same merge as `!sms merge --yes` (the app's confirmation dialog is
the confirmation step) and answers with a `merge_threads.result` queue command. A
refused selection changes nothing and only produces the error result. The `send_id` is
an app-chosen correlation id echoed in the result; it is optional but recommended.
When the phone cannot resolve the SIM for a thread it reports `sub_id: -1`; the bridge
then matches the portal by thread id instead of rebuilding the device-scoped portal key.
Other events: `send.result` (outcome for a queue command: `{send_id, message_id, thread_id,
parts_sent, pending}` or `{send_id, error: {code, message}}`) and `ack` (a non-send queue
command was handled: `{seq, type: "ack", send_id: "ack-<command_seq>", command_seq, thread_id, sub_id,
timestamp_ms}` plus an optional `error: {code, message}` and, for a failed `fetch_message`,
the `message_id` of the command; `command_seq` is optional and
carries the acknowledged command's queue sequence). An `ack` removes the matching
queued command (`send_id`, the `ack-<seq>` suffix, or `command_seq` when present) so it is
never redelivered;
unknown event types are stored in the device inbox and ignored. Response:
`{"cursor": 17, "accepted": 3}`.

Incoming messages with media carry their `agentapi.Message` payload inline, and for the app
transport the attachment bytes themselves must be inlined in
`attachments[].base64` (with `content_type` and `file_name`): the bridge cannot reach the
phone over HTTP to fetch MMS parts, so an attachment without inline data is dropped and the
message is delivered text-only (with a warning in the bridge log). The adb agent transport
does the opposite — it leaves `base64` empty and serves bytes via
`GET /v1/mms/{id}/parts/{part_id}`; the bridge prefers inline `base64` when present and
falls back to that endpoint otherwise. Keep inline data within the cap in §5 (`unsupported`
beyond it); prefer to omit oversized attachments (text-only delivery) over failing the
whole `events` batch.

Replayed events (the state snapshot used for `reset: true` and startup catch-up) do **not**
carry the message payload: re-sending every recent message with inline bytes would exceed
the request size cap and waste phone bandwidth. When such an event says `has_attachment:
true` and the bridge does not have the message yet, it asks the app for the payload with a
`fetch_message` queue command instead of bridging a media-less copy. The app answers by
pushing the message as a `message.new` event with the payload and inline parts, then an
`ack` for the command. When the app cannot provide the payload (for example the message or
part is gone), it must `ack` with an `error` and the `message_id` of the command: the bridge
then mirrors the event once without media, keeping the caption/preview and adding a single
`[MMS media could not be downloaded: …]` marker. The same marker replaces media that is
unreadable or oversized, so a message is never mirrored twice and no repeated "processing
error" notice is posted.

### `POST /_smsmms/v1/heartbeat` (Bearer)
```json
{"device_id": "android-ab12", "battery_pct": 88, "default_sms_app": true,
 "permissions": {"receive_sms": true, "send_mms": true}, "app_version": "1.10.1-bridge1",
 "subscriptions": [{"sub_id": 1, "display_name": "SIM 1", "carrier": "Example Mobile",
                    "number": "+15550001111", "default_sms": true}]}
```
Used for bridge health display and to gate MMS/read features.

The optional `subscriptions` array lets the bridge learn the phone's own numbers
without manual configuration: reported numbers are merged with the `own_numbers`
network config and excluded from group participants, so the phone's own address
never becomes a ghost user in group rooms.

## 5. Behaviour rules

- The bridge treats an app-paired login exactly like an adb-agent login at the connector
  layer: same `Message`/`Event` JSON, same portal IDs (`MakePortalID(deviceScope,
  subID, threadID)`), same echo suppression (by `send_id`).
- The `message.new` echo of a queue-command send carries the command's `send_id` (and its
  `message_id` once the provider row is known, `""` while `send.result` reports `pending:
  true`). The bridge tracks the `send_id` when it dispatches the send, so the echo is
  suppressed whether it or the `send.result` is processed first; a from-me `message.new`
  without recipients is held briefly while an outbound send is unresolved. An echo (or any
  event without a usable participant list) never reconciles room membership: the bridge only
  updates members/name from a non-empty list, so an empty list can never clear a room.
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
  rejects larger sends with `unsupported`). For incoming media on the app transport the same
  cap applies to `attachments[].base64` in `message.new`; oversized parts should be omitted
  rather than failing the batch.
- Pairing API request bodies are capped at 4 MiB (`pairingMaxBodyBytes`). The app must split
  an `events` batch that would exceed the cap into several requests; a `fetch_message` whose
  payload cannot fit is answered with an `ack` error instead, and the bridge mirrors the
  message without media.

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
