# mautrix-android-sms-mms — bridge between Matrix and Android SMS/MMS

Status: **draft plan v1** (2026-10-22). Owner: microchipster. Base: forked/adapted from
`mautrix-jmpchat` (bridgev2, Go, known-good, modern).

## 1. Goal

Bridge Android SMS and MMS — including **group MMS exactly correct** (stable rooms, no
fracturing, correct sender attribution, correct participant sets), **attachments/images**
(both directions, carrier-size aware) and **tapback reactions** (incoming → Matrix
reactions) — into Matrix, in both directions, without Google services (no Play, no RCS,
works on GrapheneOS/LineageOS).

Explicit non-goals: RCS, iOS, push-notification accounts, Google Messages integration.

## 2. Decisions locked (from user)

1. **Name/location (decided)**: `mautrix-android-sms-mms` at
   `/home/user/src/repos/matrix-docker-ansible-deploy/mautrix-android-sms-mms` (own git repo,
   like `mautrix-jmpchat`). Binaries: `mautrix-android-sms-mms` (bridge) and
   `android-sms-mms-adb-agent` (agent). Ansible roles: `matrix-bridge-mautrix-android-sms-mms`,
   `matrix-android-sms-mms-agent`.
2. **Two projects in one repo**: the Matrix bridge (server) and the phone-side agent
   (installed on the host the phone is plugged into). Each gets an Ansible role in the same
   style as `matrix-bridge-mautrix-jmpchat`.
3. **Two approaches**, same bridge:
   - **A. ADB mode** (start here): phone plugged into a computer, USB ADB, optional root.
   - **B. App mode (decided)**: full **fork of Fossify Messages + mmslib** with a bridge
     mode (HTTP/SSE API + event push + read-state endpoints); no ADB needed. Go straight to
     the fork; DekuSMS forwarding is not a base and capcom6 is kept only as a reference
     implementation.
4. **Multi-device**: one Matrix account must support multiple phone logins (bridgev2
   multiple logins). Portal IDs are device-scoped; room names and the space hierarchy are
   device-aware (see §5.1).
5. **Reactions**: incoming tapbacks (`Liked "..."`) -> real Matrix reactions. Outgoing
   reactions: default **drop** (accepted, not sent); optional fallback-text mode later.
6. **Rooms**: one stable Matrix room per phone thread (thread ID-based identity), updated
   membership, never forked on participant change.
7. **History**: default backfill 30 days; `sync 7d`, `sync 100d`, `sync full` variants.
8. **Sending MMS from Matrix**: auto-fit to carrier limits (resize/compress images, size
   configurable); never fail silently.
9. **RCS**: out of scope (anti-Google/DeGoogled; no Google account for group chats).
10. **Group chats must be perfect** (hard requirement): no fractured rooms, no janky
    single-SMS fallbacks, no dropped participants/attachments. Checklist and real-device
    acceptance protocol in §5.6. The existing `eu.droogers.smsmatrix` +
    `matrix-bridge-sms` setup is inspiration only, not a baseline.

## 3. Research findings that shape the design

### 3.1 DekuSMS cloud forwarding is not sufficient (studied)

DekuSMS 0.78.0 (GPL-3, Kotlin, core in MIT lib `smswithoutborders/lib_smsmms_android`):

- Forwarding = one HTTP POST per message per `GatewayServer`, payload `RouterItem`
  (`thread_id`, `address`, `date`, `type`, `body`, `sub_id`, `tag`), no auth header, no
  stable message ID, **no MMS media**, **no recipient list** (group collapses to first
  address; upstream bug also skips the first `addr` row), no retry for non-5xx, does not
  forward failed sends.
- Sends only via RabbitMQ consumer (`{"text","to","sid"}`), subscription selection broken,
  README's status callback unimplemented, no REST API server on the phone.
- No reaction support; read state never written back to the telephony provider.
- Conclusion: usable only as a degraded text-only forwarder. Not the app-mode base.

### 3.2 ADB access realities (verified against AOSP sources)

- **Non-root `adb shell` is surprisingly capable** on AOSP-derived ROMs (GrapheneOS,
  LineageOS, Pixel): `com.android.shell` holds `READ_SMS/WRITE_SMS/SEND_SMS`, so
  `content query content://sms|mms|mms-sms/…`, `content read content://mms/part/<id>`,
  `content update` (mark read) and `service call isms` (send text SMS) all work without
  root. GrapheneOS cannot be rooted at all, so this is the only ADB path there.
- **Root** adds: direct `mmssms.db` access (`.backup` snapshots), part files from
  `app_parts/`, `su -c` provider queries, `sqlite3` if present, `inotifyd` for cheap
  near-real-time wakeups. Android 15+/AOSP uses **rollback journal, not WAL** — copy
  only via `.backup`/`VACUUM INTO`, never bare file copy.
- **Push is app-only.** Shell/root cannot subscribe to `SMS_RECEIVED`/`WAP_PUSH`:
  ADB mode is inherently poll/sweep based (2–5 s polling, or `inotifyd` wake-ups).
- **SMS send over ADB** is text-only and version-fragile (`service call isms` transaction
  codes changed 10→11; single PDU only, no multipart, no delivery reports). Termux +
  Termux:API gives proper multipart via `RUN_COMMAND` but does not persist into the
  provider, so sent messages are invisible to the phone's messaging app. An app wins.
- **MMS send has no ADB/root path.** `MmsService` is system-uid only; the app must hand an
  M-Send.req PDU to `SmsManager.sendMultimediaMessage`, and the radio's MMS APN is only
  reachable from the phone. So: MMS sending **requires an app** on the phone (default-SMS
  role strongly recommended); the bridge must surface that clearly when not available.
- Key schema facts (group correctness): `sms` rows are reassembled multipart (one row);
  `pdu.date` is seconds (SMS is ms); `addr.type` 137=FROM, 151=TO; group membership =
  `canonical_addresses` referenced by `threads.recipient_ids`; `sms._id` has no
  AUTOINCREMENT (ids can be reused) → sweep by date with overlap, never `_id`-only
  cursors; MMS notification rows (`m_type=130`) must be ignored until downloaded
  (`132`/inbox with parts); SMIL may be absent (fall back to `part.seq` order).
- Own number can leak into recipient lists when `getLine1Number()` is null (MVNO/eSIM) →
  bridge must know `own_number` from config and exclude it.
- `adb forward` (host→phone API) + `adb reverse` (phone→host webhooks) make a USB app
  gateway fully workable without Wi-Fi.

### 3.3 App-mode candidates — verdict: fork Fossify Messages (details: `docs/research-app-base.md`)

- **Fossify Messages fork + mmslib fork** — chosen base for app mode. Full-fidelity group
  MMS read (thread participants incl. TO+CC), group MMS send (multi-To; upstream defaults
  off — we force on), multi-attachment MMS (upstream sends N attachments as N MMS; we fix),
  provider read-state writeback (READ+SEEN), stable IDs (`Message.getStableId()`),
  byte-exact export path already exists (`MessagesReader`/`MmsBackup`), active upstream
  (v1.10.1, 2026-10), GPL-3, F-Droid. Add `BridgeService` FGS with HTTP API + event push
  (skeleton proven by Traccar, a Fossify Messages fork with Jetty FGS). Fork both repos;
  mmslib has no repo LICENSE → vendor as local Gradle module + LICENSE/NOTICE, pin commit.
- **capcom6/android-sms-gateway** (Apache-2.0) — best *zero-fork interim*: local Ktor REST
  + webhooks, MMS send with multiple recipients + base64 attachments, `POST /inbox/refresh`
  backfill. Gaps: receive exposes only single sender/recipient (parse `type=137` only), no
  read-state sync, own DB not provider truth, Firebase dep, not F-Droid. Option: integrate
  it as an interim app-mode transport (bridge already abstracts app clients).
- **DekuSMS** — rejected as base: forwarding is lossy (no groups/media/stable IDs), MMS
  parser drops addr rows and hardcodes single-recipient group=false; only its CI signing
  patterns are reusable.
- Also considered and rejected: own minimal companion APK (no UX benefit, same signing
  burden), KDE Connect transport (no HTTP), Traccar API (SMS-only), SmsForwarder (MMS code
  dead), Silence/QKSMS/Simple SMS (dead upstream, no MMS-capable Matrix bridge exists).

## 4. Architecture

```
┌──────────── phone ────────────┐   USB/ADB   ┌────── adb host ──────┐        ┌──── Matrix server ────┐
│ SMS app / gateway app /       │◄───────────►│  android-android-sms-mms-adb-agent   │  HTTP  │  mautrix-android-sms-mms      │
│ (fork) — threads, MMS parts   │             │  reader/poller/sender│◄──────►│  bridgev2 connector   │
└───────────────────────────────┘             └──────────────────────┘        └───────────────────────┘
        │  (app mode: webhook/API over Wi-Fi or adb reverse)  ─────────────────────────▲
        └──────────────────────────────────────────────────────────────────────────────┘
```

- **Agent** (`cmd/android-android-sms-mms-adb-agent` + `internal/agent`): single binary on the adb host.
  Owns device access, event stream, send backends, media fetch, contacts cache. Exposes a
  small authenticated HTTP/JSON API (localhost or LAN) plus an event feed. Survives bridge
  restarts; testable without Matrix.
- **Bridge** (`cmd/mautrix-android-sms-mms` + `connector/`): a bridgev2 appservice, one login per
  agent/device, speaks only the agent API. In app mode, an "agent" can be replaced by a
  direct app client (same interface), so the connector logic stays identical.
- **Ansible roles**: `matrix-bridge-mautrix-android-sms-mms` (server) and
  `matrix-sms-mms-agent` (adb host), same conventions as
  `roles/custom/matrix-bridge-mautrix-jmpchat/` (self-build from repo, systemd unit,
  config templates, docs page, CHANGELOG entry per playbook rules).

### 4.1 Agent send backends (swappable, detected at runtime)

| Backend | SMS send | MMS send | Multipart | Notes |
|---|---|---|---|---|
| `app-api` (our Fossify fork) | yes | yes | yes | the target: true push + full fidelity + read-state sync |
| `termux` | yes | no | yes | needs Termux+Termux:API; sent rows not persisted |
| `shell` (`service call isms`) | yes | no | no | non-root fallback; short texts only; version-fragile |

The bridge exposes capabilities to Matrix based on the active backend (e.g. if MMS send
is unavailable, attachment sends in MMS conversations are rejected with a clear message
instead of failing weirdly).

### 4.2 Real-time strategy

- Root: `inotifyd` watch on the telephony DB dir (spool file read via `adb exec-out`) plus
  a 30 s safety sweep.
- Non-root: 2–5 s keyset/date sweep (`content query`, inkrementell by date overlap).
- App mode: webhooks (true push), plus a 5–10 min sweep for gap recovery.
- All modes: per-device watermark `(device, sub_id, last_date, last_id)`, overlap window,
  content-hash dedupe (see §5.2).

## 5. Correctness spec (the "no fracturing, no weird errors" part)

### 5.1 Identity & rooms

- **Portal ID** = `thread-<devicescope>-<sub_id>-<thread_id>` (`MakePortalID`), where
  `devicescope` is a short stable hash of the agent's device serial, assigned at login.
  The phone's thread is the stable conversation identity: participant changes update room
  metadata/membership instead of creating a new room. Fallback `addr-<devicescope>-<number>`
  for rooms created before the phone has a thread.
- **Multi-device**: bridgev2 already supports multiple logins per Matrix user (one login
  per phone/agent). Requirements: (a) portal IDs are device-scoped so two phones never
  collide; (b) each login gets its own space tree (`PersonalFilteringSpaces` per login,
  named after the device label from login config); (c) room names get a device suffix when
  the user has more than one login (e.g. "Alice (+1555…, Pixel 8)") and always carry a
  device tag in the room topic; (d) ghost user IDs are shared per address (same person
  across devices = same ghost), while portal rooms stay per-device.
- **Ghost IDs** = escaped canonical addresses (`MakeUserID`, `=2b`/`=2c` escaping kept
  from jmpchat so Synapse accepts `+`-containing user IDs).
- DM rooms: one other participant. Group rooms: all recipients except own number(s)
  (configured `own_numbers`, not `getLine1Number()` which is often null).
- **Group membership updates** flow via `simplevent.ChatInfoChange`; `GetChatInfo` always
  returns the full desired state (participants from `canonical_addresses`), so removals
  de-ghost/leave correctly.
- Message senders come from `addr.type=137` (incoming MMS) or `address` (SMS); outgoing
  rows (`type=2`, `msg_box=2`) are "me" — never attribute them to the thread's address.

### 5.2 Event pipeline & dedupe

Order of operations per remote event:

1. Ignore notification-only MMS rows (`m_type=130`), drafts (`msg_box=3`).
2. Build/dedupe key: SMS `(sub_id, address, date_sent_ms, body_hash)`; MMS
   `(sub_id, thread_id, date_sent_s, m_id)` + part hashes. Dedupe TTL ~7 days; carrier
   redelivery is real.
3. Ensure portal (`ChatResync` with `CreatePortal: true`) → then `simplevent.Message`
   with `ConvertMessageFunc` for text + uploads.
4. Echo suppression: agent tags messages it sent itself (pending-send table with
   recipients+body+time window), so the DB row that appears afterwards is not re-imported
   as a new message; unmatched outgoing rows from the phone itself are forwarded and
   matched by content if a Matrix-origin send raced.
5. All timestamps normalized to the Matrix event (SMS ms, MMS s).
6. Failures are surfaced: agent health errors → bridge notice in management room; one
   bad attachment never drops the text part.

### 5.3 Attachments

- MMS parts: download via `content read`/`_data` (root) or provider (`content read`;
  non-root restricted view still allows part reads), map `cid`/`cl`/`name` via SMIL when
  present else `seq` order; skip `application/smil` itself; treat `text/plain` as body.
- Missing part file (garbage-collected) → text-only message + note, never a hard error.
- Upload via `intent.UploadMedia` with proper `msgtype`/`FileInfo` (jmpchat pattern).
- Outgoing: auto-fit pipeline in the agent (JPEG re-encode, long-edge limit), preserving
  originals in Matrix; size cap from agent config; HEIC→JPEG; video passthrough if under
  cap else clear error/link fallback; one image per MMS par for reliability.
- `GetCapabilities` advertises what the active backend can actually do.

### 5.4 Reactions

- Port `xmpp_tapback.go` parsing (`Liked/Loved/Laughed at/Emphasized/Questioned/Disliked`
  + quote extraction) → match against stored `MessageMetadata.Body` (+ attachment-aware
  rules) → `simplevent.ReactionSync`.
- Outgoing reactions: accept, drop by default; optional config
  `send_reactions_as_text` formats `Reacted 👍 to "…"`.

### 5.5 History / backfill

- `historySyncStartForPortal` logic ported from jmpchat: `LastSyncedAt`-overlap else
  default `now - 30d`; commands `sync`, `sync 7d`, `sync 100d`, `sync full`.
- Progress marked from `PostHandleFunc` via `markPortalHistoryProgress` (ported) and
  `LoginMetadata.LastSyncAt`.
- Backfill batches through `FetchMessages`/`BackfillMessage` with `HasMore`.

### 5.6 Group-chat perfection checklist (hard requirement)

Fidelity rules (each gets a test + a real-device check before release):

1. **Room identity**: Matrix room <-> `(device, sub_id, thread_id)`. Participant changes
   update the room (name, members, topic); they never create a second room. Reinstalls or
   phone restore that change thread IDs are handled by an explicit `rebind-room` command,
   with a warning listing orphaned rooms.
2. **Participants**: read FROM (137), TO (151) and CC (130) from `addr`; dedupe; exclude
   own numbers (config `own_numbers`, not `getLine1Number`); never let the merged `a|b`
   SMS dummy address row leak into rooms.
3. **Sender attribution**: incoming = FROM (MMS) / `address` (SMS); sent rows are always
   "me". Replies to a removed participant still attribute the message to the phone number
   that actually sent it.
4. **Sending**: group sends are one MMS with multiple To recipients and all attachments in
   one message (never N messages, never per-recipient loops); the bridge refuses to send a
   "group" message as individual SMS; carrier rejection of group MMS is surfaced in Matrix
   with the exact error, and the phone-side fork retries via the normal MMS machinery.
5. **Threading/UX**: message body text, subject (MMS), attachments (all parts, in SMIL
   order), links, emoji and tapbacks round-trip; no double-posting; no ghost duplicates;
   edits/deletes from the phone (happens rarely) are handled as re-import updates or notes.
6. **Read state**: opening the Matrix room marks the thread read on the phone (app mode);
   unread counters never reset from the bridge side while a send is in flight.
7. **Regression suite for known app bugs**: Fossify #744 (quick reply -> last participant),
   #709 (group from Contacts -> first member only), #595 (reply to unrelated recipient),
   #167 (text-only group MMS dropped), #796 (duplicate outgoing rows), #863 (incoming MMS
   spawning a new conversation); each must be covered by a fixture or an integration test.
8. **Acceptance protocol (before every release)**: on GrapheneOS (non-root, app mode) and
   LineageOS (root, ADB mode): create a 3+ participant group; send text, emoji, a photo,
   two photos in one MMS, and a long multipart SMS from the phone; send the same from
   Matrix; receive from a non-participant; have a participant leave and rejoin; restart
   the phone and the bridge mid-conversation; verify identical participant lists, zero
   duplicate rooms, zero duplicate messages, and correct timestamps in all cases.

## 6. Agent HTTP API (v1 draft)

Bearer token; JSON; localhost by default.

```
GET  /v1/health                  → device, android, root?, backends[], default_sms_app, own_numbers
GET  /v1/subscriptions           → [{sub_id, number, carrier, default_sms}]
GET  /v1/threads                 → [{thread_id, sub_id, recipients[], snippet, last_date, unread}]
GET  /v1/threads/{id}/messages?before=&after=&limit=
GET  /v1/messages/{sms_id}       → full SMS row
GET  /v1/mms/{pdu_id}            → pdu + addr[] + part[] (with text inline, binaries by URL)
GET  /v1/mms/{pdu_id}/parts/{id} → binary (Content-Type from part.ct)
GET  /v1/events?since=<cursor>   → SSE or long-poll: message.new, message.updated, mms.downloaded, thread.updated
POST /v1/send                    → {recipients[], text?, attachments[{part_id|base64}], thread_id?, sub_id?} → {message_id, thread_id}
POST /v1/read                    → {thread_id?, message_ids[]}
GET  /v1/contacts?q=             → cached phonebook lookup for display names
```

Contract rules: all addresses E.164-normalized by the agent; timestamps in ms; stable
`message_id` = `<sub_id>-<sms|mms>-<row_id>` (namespaced per device by the bridge);
idempotent send (client-supplied `send_id`); no truncation — large parts stream.

Bridge↔agent failure handling: agent unreachable → login marked degraded, sends refused
with clear Matrix notice; on agent recovery, watermark catch-up runs automatically.

## 7. Recycling map from mautrix-jmpchat

| jmpchat file | new file | change |
|---|---|---|
| `cmd/*/main.go` | `cmd/mautrix-android-sms-mms/main.go` | **done** (names; management-room encryption gate kept) |
| `cmd/*/room_encryption.go` + test | same | **done** (verbatim) |
| `cmd/*/management_room.go` + test | same | **done** (verbatim) |
| `connector/types.go` | `connector/types.go` | **done** (SMS/MMS metadata: agent URL/token/device, thread/sub_id/recipients, body/attachment) |
| `connector/jid.go` | `connector/phoneid.go` | **done** (phone canonicalization, short codes, sender IDs, emails; escaping kept; thread-based portal IDs added) |
| `connector/history_sync.go` | same | port; swap message-id type to agent IDs |
| `connector/xmpp_tapback.go` | `connector/tapback.go` | port parser; new SMS-side matching |
| `connector/xmpp_attachment.go` | `connector/media.go` | keep `classifyContentTypeMessageType` + caption helpers; download/upload via agent |
| `connector/handle_matrix.go` | same | port structure: `HandleMatrixMessage` → agent send; `GetChatInfo` → thread recipients; `GetCapabilities` → backend-dependent; `GetUserInfo`/naming helpers kept |
| `connector/network_client.go` | same | rewrite transport; keep simplevent/ConvertMessageFunc patterns |
| `connector/login.go` | same | flows: "ADB agent" (URL+token), later "App gateway" |
| `connector/my_connector.go` | same | keep structure; new command handlers |
| `connector/commands.go` | same | port generic subset first: `ping`, `sync`, `sync-status`, `list-logins`, `list-users`, `delete-all-portals`, `logout`, plus SMS-specific `send`, `contacts` |
| `Dockerfile`, `docker-run.sh`, `build-go.sh`, `.mise.toml`, `.gitignore`, `.dockerignore`, `example-config.yaml` | same | adapt names/paths |
| docs (`AGENTS.md`, `DEVEL.md` skeleton, `MEMO.md` rules) | `docs/` | rewrite for SMS domain |

New code: `internal/agent/` (adb access, provider queries, smil, mms parts, pollers,
senders), `connector/agentclient.go` (HTTP client + event feed), and the app-mode client.

## 8. Phases

**Phase 1 — ADB read path (no sending yet).**
Agent skeleton + provider readers + poller + media fetch; bridge login (agent URL/token),
threads → rooms, history backfill 30d, tapback reactions, agent health notices.
Exit: SMS + MMS (incl. group) from a real phone land correctly in Matrix, with attachments
and correct senders/participants; no duplicate rooms on restart.

**Phase 2 — SMS sending.** `service call isms` (short) + Termux backend (multipart) +
read-state write-back. Exit: Matrix→phone SMS works both directions with echo suppression
proven by tests; long SMS handled (split via Termux or documented limitation).

**Phase 3 — App mode (MMS sending + true push).** Fork Fossify Messages + mmslib per
`docs/research-app-base.md` (BridgeService FGS + HTTP API, event-push hooks in
SmsReceiver/MmsReceiver, provider-backed history/send, group flag forced on, N attachments
in one MMS, read-state endpoints). Auto-fit image pipeline; group MMS send; delivery
states. Exit: sending a group MMS with images from Matrix works end-to-end; read state
syncs.

**Phase 4 — App mode without ADB.** App webhooks + API as first-class transport; same
connector; docs for both setups. Exit: feature parity smoke test in app mode.

**Phase 5 — Playbook integration + polish.** Two Ansible roles, docs pages, CHANGELOG
entries (including the existing `matrix-bridge-sms` deprecation note if we replace it),
packaging (container image, local preload), release tagging.

## 9. Testing

- Unit: phone canonicalization table tests; tapback parse cases (incl. smart quotes,
  no-quote, attachment targets); SMIL/part mapping fixtures; dedupe/echo logic;
  size-fit pipeline.
- Fixture DBs: checked-in SQLite fixtures (SMS multipart, MMS group with TO/CC/FROM,
  notification-only row, email/short-code senders, own-number-leak case) driven through
  the agent parser.
- Fake agent: in-process HTTP agent implementing the API for bridge integration tests
  (bridgev2 test harness), including restart/lag scenarios.
- Emulator: AOSP emulator image with `adb root`; scripted insert of SMS/MMS rows via
  provider; real adb paths exercised in CI where possible (`content query` grammar,
  `exec-out` binary safety).
- Manual acceptance matrix on real devices before each release: GrapheneOS (non-root),
  LineageOS (root), dual SIM, group with 3+ participants, reactions, images, long SMS.

## 10. Resolved decisions & remaining questions

Resolved:

1. **App base**: full fork of Fossify Messages + mmslib (no capcom6 interim) - user
   confirmed.
2. **Signing flow**: existing canonical flow reused as-is - `android-signing-keys` script
   stores keystore + password in `pass` at `<namespace>/android/<applicationId>/`,
   symlinks `app/release.keystore`, writes `~/.config/android/<applicationId>/storepass`;
   Makefile passes `ANDROID_STOREPASS` to Gradle. Keys already in pass for existing apps;
   nothing to migrate. See `docs/SIGNING.md` for the fork-specific plan (key namespace,
   applicationId decision, CI signing, release channel).
3. **Repo name**: `mautrix-android-sms-mms` (binaries/roles as in §2).
4. **Dual SIM**: model both SIMs separately (`sub_id` in portal IDs and message keys);
   the bridge will additionally offer `sync`/room naming that makes the SIM visible, and
   we can add cross-SIM thread merging later if it turns out to matter.
5. **Existing `matrix-bridge-sms`**: ignored; kept only as inspiration.

Remaining:

1. **Phone/ROM targets**: which devices/ROMs must work? Current assumption: GrapheneOS
   (non-root app mode) + LineageOS (root ADB mode). Confirm.
2. **Fork applicationId strategy**: drop-in (`org.fossify.messages`, replaces the F-Droid
   build - uninstall first, provider history survives) vs side-by-side
   (`org.fossify.messages.bridge` or similar, both installable, fork must be set as default
   SMS app to receive). Recommendation: drop-in for daily use after a side-by-side trial
   phase.
3. **Release channel**: private F-Droid repo vs Obtainium vs plain `adb install` for the
   fork; CI system (Forgejo Actions? GitHub Actions?) for signed builds.
4. **Own numbers**: exact `own_numbers` list per SIM to exclude from participants.
