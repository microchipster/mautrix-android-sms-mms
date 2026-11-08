<!--
SPDX-FileCopyrightText: 2026 Slavi Pantaleev

SPDX-License-Identifier: AGPL-3.0-or-later
-->

# Validation harness (`scripts/validate/`)

The per-repo end-to-end harness for the SMS/MMS bridge, the adb agent and the app fork.
It is env-driven: nothing assumes a specific workstation, phone or deployment. `t01` is the
smoke test that must report no FAILs before any of the acceptance tests run; `t02`–`t14`
follow the acceptance matrix in `TODO.md` and always assert real state (provider rows,
bridge channel ids, room membership, timestamps, SHA-256 hashes).

## Contents

| File | Purpose |
| --- | --- |
| `scripts/validate/lib.sh` | environment contract + helpers, sourced by every test |
| `scripts/validate/t01_local_rig.sh` | smoke: adb + bridge + agent + app control surface |
| `scripts/validate/t02_pair.sh` | pairing API, app paired, heartbeat freshness, `!sms devices` |
| `scripts/validate/t03_inbound_1to1.sh` | inbound 1:1 SMS → portal, sender ghost, own-number filter |
| `scripts/validate/t04_group_create.sh` | one room per group thread, second sender, no fracture |
| `scripts/validate/t05_attachments.sh` | byte-exact attachments (SHA-256 both ways), MMS size limit |
| `scripts/validate/t06_group_send.sh` | group send → exactly one MMS row, no SMS fallback, no new room |
| `scripts/validate/t07_membership.sh` | membership in place: one room per channel id, no dupes/own-number ghost |
| `scripts/validate/t08_tapbacks.sh` | tapbacks both ways (inbound reactions; opt-in outgoing text fallback) |
| `scripts/validate/t09_read_state.sh` | read state reaches the phone (`read=1 seen=1` on inbound SMS) |
| `scripts/validate/t10_restart_dedupe.sh` | bridge DB dedupe + optional bridge/app restarts keep portals, recipients and phone rows |
| `scripts/validate/t11_nonroot.sh` | app transport works without root; agent root/non-root report |
| `scripts/validate/t12_multidevice.sh` | device inventory + 2/3/4-participant group combos |
| `scripts/validate/t13_group_events.sh` | group reactions/uploads/replies and SMS+MMS fallback evidence |
| `scripts/validate/t14_invite_join.sh` | no stray invites, management room/portals joined |
| `scripts/validate/t15_message_types.sh` | SMS message-type matrix: plain, long multipart and unicode/emoji, both directions |
| `scripts/validate/t16_gif_roundtrip.sh` | animated GIF both directions: content-type + byte-exact SHA-256 |
| `scripts/validate/t17_multi_attachment.sh` | one multi-attachment send → exactly one MMS with both image parts |
| `scripts/validate/t18_media_variants.sh` | video (inbound) and audio (both directions) variants with degradation reporting |
| `scripts/validate/t19_unknown_sender.sh` | unknown-number thread/portal creation, no duplicate threads |
| `scripts/validate/t20_cdn.sh` | bridge CDN file-service contract: crypto round trips, wrapper headers, expiry, quotas, revoke, isolation |
| `scripts/validate/t21_call_capture.sh` | two-phone SIM call + rooted in-call capture on the callee (precheck → dial → ACTIVE → tinycap → setup-failure evidence) |
| `scripts/validate/fixtures/cat.gif` | tiny animated cat for the attachment tests (generated in-house) |
| `scripts/validate/fixtures/paw.png` | second image for the multi-attachment test (generated in-house) |
| `scripts/validate/fixtures/tiny.mp4` + `tiny.m4a` + `tiny.ogg` | synthetic video/audio fixtures (generated in-house) |
| `scripts/validate/fixtures/tiny.png` + `tiny.pdf` | deterministic CDN round-trip fixtures (generated in-house) |
| `scripts/validate/fixtures/make-cat-gif.sh` | fixture generator + regeneration docs |
| `scripts/validate/fixtures/make-media-fixtures.sh` | generator for `paw.png`/`tiny.*` + regeneration docs |
| `scripts/validate/fixtures/make-cdn-fixtures.sh` | generator for `tiny.png`/`tiny.pdf` + regeneration docs |

## Quick start

```sh
# 1. local dev rig (defaults, see docs/testing-e2e-local.md)
./scripts/validate/t01_local_rig.sh
./scripts/validate/t02_pair.sh

# 2. a deployment from the host the phone is plugged into (set ADB_SSH):
#    (t01 SKIPs whatever is unreachable — run it here too if the bridge route works)
BRIDGE_URL=https://matrix.example.com \
  ./scripts/validate/t01_local_rig.sh
```

Deployment acceptance runs need the adb host, the iamb host and the Matrix room ids:

```sh
export BRIDGE_URL=https://matrix.example.com
export ADB_SSH=<ssh-host> ADB_SERIAL=<adb-serial>          # adb runs on <ssh-host>
export APP_PACKAGE=org.fossify.messages.bridge.debug
export CONTROL_TOKEN="$(cat /path/to/chatty/control-token)"   # never commit this
export IAMB_SSH=<ssh-host> IAMB_PROFILE=public
export SMS_MGMT_ROOM='!<bridge management room id>'
export PHONE_NUMBER='+1628…'                       # the phone's own E.164 number
export IAMB_PHONE_ROOM='!<iamb room that texts this phone>'         # live inbound probe
export IAMB_GROUP_ROOM='!<iamb group room with the phone + friends>' # live group probe
export IAMB_GROUP_PORTAL='!<Matrix portal room of a group thread>'   # live group send
export ATTACHMENT_TARGET_NUMBER='+1…'              # outbound attachment target

./scripts/validate/t02_pair.sh
./scripts/validate/t04_group_create.sh
# live probes (real SMS/MMS — keep the total small):
VALIDATE_LIVE_SENDS=1 ./scripts/validate/t03_inbound_1to1.sh
VALIDATE_LIVE_SENDS=1 ./scripts/validate/t04_group_create.sh
VALIDATE_LIVE_SENDS=1 ./scripts/validate/t05_attachments.sh
```

Every script exits `0` when nothing reachable misbehaves and `1` on FAILs. A SKIP never
fails a run: it means the component, the environment or the opt-in live probe is absent.

## Environment contract (`lib.sh`)

| Variable | Default | Meaning |
| --- | --- | --- |
| `ADB_SERIAL` | required | adb serial of the phone under test. |
| `ADB_SSH` | empty | run adb on that host: `ssh $ADB_SSH adb …` (e.g. `<ssh-host>`). |
| `APP_PACKAGE` | `org.fossify.messages.bridge` | app under test (`.debug` for the debug build). |
| `CONTROL_TOKEN` | empty | token extra for control broadcasts; never logged. |
| `PHONE_TELEPHONY_DB` | `/data/data/com.android.providers.telephony/databases/mmssms.db` | provider DB queried via `adb shell sqlite3`. |
| `PHONE_SQLITE_BIN` | `sqlite3` | sqlite3 binary on the device. |
| `PHONE_SQLITE_MODE` | `auto` | `auto` retries with `su -c` when the direct query fails. |
| `PHONE_DOWNLOAD_DIR` | `/sdcard/Download` | shared push location. |
| `PHONE_ATTACHMENT_DIR` | `/sdcard/Android/data/$APP_PACKAGE/files` | app-readable dir for `control.SEND` attachments (scoped storage). |
| `PHONE_NUMBER` | empty | the phone's own E.164 number (own-number ghost checks). |
| `BRIDGE_URL` | `http://127.0.0.1:29322` | bridge base URL (deployment: reverse-proxied origin). |
| `BRIDGE_TOKEN` | empty | optional bearer token for bridge endpoints. |
| `SMSMMS_INFO_PATH` | `/_smsmms/v1/info` | bridge info path (`docs/PAIRING_PROTOCOL.md` §4). |
| `SMS_MGMT_ROOM` | empty | bridge `!sms` management room id (bot-command checks via iamb). |
| `AGENT_URL` | `http://127.0.0.1:8093` | adb agent base URL. |
| `AGENT_TOKEN` | required | agent bearer token; `/v1/health` is auth-free. |
| `AGENT_INFO_PATH` | `/v1/health` | agent info path. |
| `CURL_TIMEOUT` | `10` | seconds per HTTP request. |
| `IAMB_SSH` | required | ssh host running iamb. |
| `IAMB_BIN` | `$HOME/.cargo/bin/iamb` | remote iamb binary (literal `$HOME` expands on the host). |
| `IAMB_PROFILE` | `public` | iamb profile. |
| `IAMB_NO_IPC` | `0` | `1` inserts the global `--no-ipc` **before** the subcommand. |
| `IAMB_SEND_TIMEOUT` | `90` | seconds for `iamb_send_wait`. |
| `BRIDGE_MATRIX_URL` / `BRIDGE_MATRIX_TOKEN` | empty | future direct Matrix polling (`wait_for_room`). |
| `VALIDATE_LIVE_SENDS` | `0` | `1` enables the opt-in real SMS/MMS probes. |
| `IAMB_PHONE_ROOM` | empty | iamb room that texts the phone (live inbound probes: t03/t05). |
| `IAMB_GROUP_ROOM` | empty | iamb group room with the phone + friends (t04/t07/t14). |
| `IAMB_GROUP_PORTAL` | empty | Matrix portal room of a group thread (t06/t13). |
| `IAMB_PORTAL_ROOM` / `IAMB_TAPBACK_EVENT` | empty | portal room + event id for the outbound tapback probe (t08). |
| `ATTACHMENT_TARGET_NUMBER` | empty | recipient for the outbound attachment probe (t05). |
| `MMS_SIZE_LIMIT_BYTES` | `307200` | carrier MMS limit the fixture must fit. |
| `PHONE_EXPECT_READ` | `0` | `1` makes unread inbound SMS a FAIL in t09. |
| `PHONE_HEARTBEAT_MAX_AGE_MS` | `1800000` | heartbeat staleness limit (t02/t11). |
| `PORTAL_STRICT_PARTICIPANTS` | `0` | `1` requires portals to name every phone participant (t04/t07). |
| `EXPECT_REACTION_TEXT` | `0` | `1` asserts the `send_reactions_as_text` fallback on the phone (t08). |
| `INVITES_ACCEPT` | `0` | `1` lets t14 accept pending invites instead of failing on them. |
| `BRIDGE_DB` | local E2E rig's `bridge.db` when present | bridge database for dedupe checks (t10). |
| `BRIDGE_RESTART_CMD` | empty | command t10 runs to test restart dedupe (never set for production). |
| `APP_RESTART` | `0` | `1` lets t10 restart the paired app and assert no phone rows are replayed. |
| `EXPECT_PORTAL_MIRROR` | `1` | `1` makes a live probe whose phone-side message never reaches its Matrix portal a FAIL; `0` downgrades that to a SKIP with evidence (t15–t18). |
| `PORTAL_MIRROR_WAIT` | `60` | seconds to wait for a portal mirror before failing/skipping. |
| `SMS_TARGET_NUMBER` | `$JMP_NUMBER` | outbound target for the SMS message-type probes (t15). |
| `MEDIA_TARGET_NUMBER` | `$SMS_TARGET_NUMBER` | outbound target for the attachment/media probes (t16–t18). |
| `MEDIA_BYTE_EXACT_INBOUND` | `1` | `1` makes an inbound video/audio hash mismatch a FAIL; `0` documents the transport degradation as a SKIP. |
| `APP_BRIDGE_URL` / `APP_BRIDGE_TOKEN` | empty | pre-existing app HTTP API base URL + bearer token for t17 (token never logged). |
| `APP_BRIDGE_PORT` | `8391` | app HTTP API port that t17 forwards over adb when it enables the API itself. |
| `APP_HTTP_ENABLE_TEMP` | `0` | `1` lets t17 temporarily enable the app's static-token HTTP mode via `control.CONFIG` and restore it afterwards. |
| `IAMB_NEW_SENDER_ROOM` | empty | iamb room that texts the phone from a genuinely new number (t19 live probe). |
| `NEW_SENDER_NUMBER` | `$SMS_TARGET_NUMBER` | number whose phone thread/portal t19 inspects. |
| `MAX_DM_THREADS` | `20` | cap on 1:1 threads t19 walks for portal coverage. |
| `MAX_GROUP_THREADS` / `MAX_CANDIDATE_ROOMS` | `3` / `6` | caps for slow iamb/portal scans. |
| `CDN_BASE_URL` | empty | live CDN file-service base URL, e.g. `https://cdn.example.com` (t20); without it the whole matrix SKIPs. |
| `CDN_TOKEN` | empty | uploader's paired-device bearer token (t20; never logged). |
| `CDN_TOKEN_2` | empty | second user's token for the cross-user isolation check (t20; that check SKIPs without it). |
| `CDN_EXPIRES` | `12h` | expiry sent with the round-trip uploads (t20; must not exceed `cdn.max_expiry`). |
| `CDN_SHORT_EXPIRES` | `60s` | short expiry proven end-to-end, then asserted as 404 on read (t20). |
| `CDN_EXPIRY_WAIT` | `5` | extra seconds to wait past `CDN_SHORT_EXPIRES` (t20). |
| `CDN_MAX_FILE_BYTES` | empty | configured `cdn.max_file_bytes`; enables the oversized-upload 413 probe (t20). |
| `CDN_PER_USER_CAP_BYTES` | empty | configured `cdn.per_user_cap_bytes`; enables the FIFO-pruning probe (t20). |
| `CDN_MAX_UPLOAD_BYTES` | `8388608` | probe guard: caps above it SKIP with a note (t20). |
| `CDN_CHECK_DISCOVERY` | `1` | `1` asserts bridge `_smsmms/v1/info` advertises a matching `cdn_base_url` (t20). |
| `CDN_INSECURE` / `CDN_NODE_TLS_INSECURE` | `0` | `1` lets curl / Node accept self-signed staging certificates (t20). |
| `CDN_PYTHON` / `CDN_NODE` | `python3` / `node` | interpreter overrides for the crypto helpers (t20). |
| `PHONE_A_SERIAL` / `PHONE_B_SERIAL` | required | adb serials for t21: A = caller, B = callee/capture phone. |
| `PHONE_A_SSH` / `PHONE_B_SSH` | empty | run adb for that phone on the named ssh host (empty = local adb) (t21). |
| `PHONE_A_NUMBER` / `PHONE_B_NUMBER` | empty / required | A's own E.164 (matches the SIM row of the "Choose SIM" picker) and the number t21 dials on B. |
| `PHONE_B_MIXER_DL` / `PHONE_B_MIXER_UL` / `PHONE_B_PCM_DEVICE` | per-device | in-call capture route on B (`VOC_REC` mixer controls + ALSA PCM); defaults for the two lab phones, else derived from the control name (t21). |
| `E3_DIR` | `/data/local/tmp/e3` | rooted tinyalsa staging on both phones (t21). |
| `CALL_ACTIVE_TIMEOUT` | `45` | seconds t21 waits for the call to reach ACTIVE. |
| `CAPTURE_SECONDS` | `10` | tinycap capture length in seconds (t21). |
| `PICKER_WAIT` | `15` | seconds t21 waits for/handles the "Choose SIM"/account picker (t21). |
| `KEEP_TMP` | `0` | `1` keeps t21's host work dir and the device WAV for inspection. |

Tokens never belong in committed files; pass them via the environment (or a local,
gitignored `.env` sourced by your shell).

## Bridge CDN matrix (`t20_cdn.sh`)

`t20_cdn.sh` exercises the bridge-hosted encrypted file service
([cdn-file-service.md](cdn-file-service.md)) the way the app does: client-side
AES-256-GCM encryption (`nonce||ct||tag`, 32-byte key in the URL fragment), a
bearer `POST /f`, an anonymous `GET /f/<id>/raw` + `GET /f/<id>` fetch, a local
decrypt, wrapper-page headers, expiry, quotas, revoke and cross-user isolation.
`CDN_TOKEN` is the **paired device's bearer token** (the same one the app
uses for `/_smsmms/` pairing flows); it is never printed. The script SKIPs
cleanly when `CDN_BASE_URL`/`CDN_TOKEN` are unset, and every other component
SKIPs with a documented reason when it cannot be probed.

Live invocation (the `cdn.example.com` case):

```sh
export CDN_BASE_URL=https://cdn.example.com
export CDN_TOKEN="$(cat /path/to/pairing-device-token)"    # never commit
export CDN_TOKEN_2="$(cat /path/to/second-device-token)"   # isolation checks
BRIDGE_URL=https://<homeserver> ./scripts/validate/t20_cdn.sh
```

Feature-gated notes:

- The 413 size probe and the FIFO per-user-cap probe need the *actual* caps of
  the deployment you point at: pass `CDN_MAX_FILE_BYTES`/`CDN_PER_USER_CAP_BYTES`
  when they are small enough to probe over the network (≤ `CDN_MAX_UPLOAD_BYTES`,
  default 8 MiB), e.g. a staging deploy with a 1 MiB file cap. On the production
  defaults (100 MiB / 1 GiB) both checks SKIP with the observed `cap_bytes`.
- The post-expiry check uses `CDN_SHORT_EXPIRES` (default 60s) and asserts the
  read path 404s; the hourly `cdn gc` that frees the bytes is admin-only and is
  not triggered over HTTP.
- The discovery check asserts `GET <BRIDGE_URL>/_smsmms/v1/info` advertises a
  `cdn_base_url` equal to `CDN_BASE_URL` (`CDN_CHECK_DISCOVERY=0` to skip).
- Self-signed staging deployments: `CDN_INSECURE=1` (curl) and
  `CDN_NODE_TLS_INSECURE=1` (Node WebCrypto re-check).
- When `node` is unavailable, the WebCrypto re-check SKIPs and the structural
  wrapper checks (headers, data-only id, no key/plaintext) still run.

Local, without a deployment: `go test ./... -run Cdn -count=1` covers the same
HTTP contract in-process, including the app's AES-256-GCM steps
(`connector/cdn_appcontract_test.go`).

## Phone-SIM call capture (`t21_call_capture.sh`)

`t21_call_capture.sh` proves the rooted in-call capture path from
[cell-call-over-matrix-research.md](cell-call-over-matrix-research.md) §6 E2/E3
end to end: phone A dials `PHONE_B_NUMBER` (`am start … ACTION_CALL`; the
`cmd telecom set-user-selected-outgoing-phone-account` default account when the
build supports it, else the "Choose SIM" picker is tapped via uiautomator), B
answers, and the script waits `CALL_ACTIVE_TIMEOUT` for ACTIVE. On an ACTIVE
call it flips B's `VOC_REC_DL` mixer route On, captures `CAPTURE_SECONDS` with
the staged `/data/local/tmp/e3/tinycap` (single channel, 48 kHz, 16-bit),
verifies the WAV size and non-silence (sox stat, else a python3 RMS helper) and
restores the mixer. A setup failure FAILs with the exact carrier/IMS evidence
(telecom `ImsReasonInfo`, radio reason codes, callee-never-rang verdict) instead
of timing out silently; every run ends with mixers Off and calls ended.

```sh
PHONE_A_SERIAL=<caller-adb-serial>  PHONE_A_SSH=<host> PHONE_A_NUMBER='+1…' \
PHONE_B_SERIAL=<callee-adb-serial>  PHONE_B_SSH=<host> PHONE_B_NUMBER='+1…' \
./scripts/validate/t21_call_capture.sh
```

Live invocation notes: the capture phone must carry the `$E3_DIR` staging
(tinycap/tinymix) and root; route defaults exist for the two lab devices
(OnePlus 9 Pro → `MultiMedia9 Mixer VOC_REC_DL/UL`, PCM 23; Pixel 2 XL →
`MultiMedia1 Mixer VOC_REC_DL/UL`, PCM 0) and can be overridden with
`PHONE_B_MIXER_DL`/`PHONE_B_MIXER_UL`/`PHONE_B_PCM_DEVICE`. Audio never leaves
the devices: only ids, sizes, timestamps and level/rms numbers are logged, and
the temporary host copy is removed unless `KEEP_TMP=1`.

Acceptance run 2026-10-27: both lines reproduce a carrier/IMS setup cancellation
(`339 CODE_SIP_REQUEST_CANCELLED`, SIP 487, "ocs terminated the call"; callee
never rings) — the script FAILs there by design and will complete the capture
automatically once a call connects.

## Pointing at the local rig vs a deployment

- **Local rig** (defaults): bridge pairing API on `http://127.0.0.1:29322`, agent on
  `http://127.0.0.1:8093`, phone on adb. Nothing to set — see
  [testing-e2e-local.md](testing-e2e-local.md) to bring the rig up.
- **Deployment**: set `BRIDGE_URL=https://matrix.example.com` for a deployed bridge (the
  test deployment; the production deployment works the same way). A deployed `/_smsmms/` is
  served through the reverse proxy on 443, not on the container's `:29322` listener. The
  agent and adb checks only mean something on the host the phone is plugged into, so set
  `ADB_SSH=<ssh-host>` and run the tests from a machine that can reach the bridge;
  `t01` SKIPs whatever is unreachable instead of failing.
- Agent deployments that run on a different host than the harness need a tunnel, e.g.
  `ssh -L 8093:127.0.0.1:8093 <ssh-host>` before running the tests.

## Status semantics

Every check reports one of:

- **PASS** — the component answered and matched the contract.
- **SKIP** — the component is absent, unreachable, the live probe is disabled, or the
  data needed for the check does not exist yet (no group thread, no MMS part, no reaction
  observed). SKIP never fails the run.
- **FAIL** — the component is reachable but answers wrong: bad `protocol_version`, missing
  `bridge_name`, `ok=false`, HTTP error, invalid JSON, unparseable control verdict, a
  duplicated bridge channel id, a missing portal message, a size limit breach, or a live
  probe that did not produce the single expected row/room.

Graceful degradation example: the installed app may predate the control intents work.
`am broadcast` then still exits 0 but prints no `data=` verdict; the harness reports
"app build without control support" as a SKIP with the raw broadcast output as the reason,
never as a failure.

## Evidence rules

- Record **ids, senders, timestamps, cursors and hashes only — never message bodies**
  (this matches the tracker rule in `docs/DEVELOPMENT.md` (conventions)).
- Use full `!room_id`s in iamb interactions; fuzzy names hang on prompts.
- Waits are 60–120 s and use `--reply-mode different-sender`.
- Attachment checks compare full SHA-256; `sha256_16` in `lib.sh` prints a short,
  readable prefix for logs (still ids/hashes only).
- Tokens are never echoed, not even truncated.

## iamb quirks (the patched CLI on `<ssh-host>`)

- Message bodies go in `--inline '<text>'` (or `--attach <file>`); `--text` only applies
  with `--react-to`. `--stdin` does not work through the daemon handoff.
- `--no-ipc` is a **global** flag and must come before the subcommand; `iamb_cmd` inserts
  it correctly when `IAMB_NO_IPC=1`.
- Public-profile archives are
  `~/.local/share/iamb/profiles/<profile>/events/<room-without-!>.ndjson`; the personal
  profile stores the same NDJSON **without** the `.ndjson` suffix. `archive_tail` tries
  both.
- Prefer `rooms <query> --format json` over `rooms .`, which can omit freshly joined rooms
  from the daemon snapshot. `--user <…>` hydrates member lists and can hang; do not use it
  in scripts.
- Bridge portal identity comes from the `m.bridge` state event's `channel.id`
  (`thread-<scope>-<sub>-<thread>`); room *names* can collide between threads, so
  one-room-per-thread checks compare channel ids, not names.

## Fixtures

`scripts/validate/fixtures/cat.gif` is a 6-frame animated GIF (32070 bytes) generated
in-house by `scripts/validate/fixtures/make-cat-gif.sh` from ImageMagick drawing
primitives — no third-party artwork. `t05_attachments.sh` and `t16_gif_roundtrip.sh`
push it to the phone, check the shared→device and device→Matrix paths byte-exact (full
SHA-256), and assert it fits `MMS_SIZE_LIMIT_BYTES`.

`scripts/validate/fixtures/make-media-fixtures.sh` generates `paw.png` (second image for
`t17_multi_attachment.sh`) and the synthetic media fixtures `tiny.mp4` (H.264 video),
`tiny.m4a` (AAC audio) and `tiny.ogg` (Opus audio) used by `t18_media_variants.sh` — all
from ImageMagick drawing primitives and ffmpeg `lavfi` sources, no third-party content.
`fixtures/README.md` documents provenance, sizes, hashes and regeneration.

`scripts/validate/fixtures/make-cdn-fixtures.sh` generates the deterministic
`tiny.png` (64x64 RGB PNG) and `tiny.pdf` (one-page PDF) used by `t20_cdn.sh` for the
binary round trips and PDF sniffing — python3 stdlib only (`struct`/`zlib`), no
third-party content; the CDN matrix also reuses `tiny.mp4` from the media fixtures.

## Test map (`TODO.md` acceptance matrix)

| # | Acceptance step | Test | Live probe (opt-in) |
| --- | --- | --- | --- |
| 1 | Pair + `!sms devices` + heartbeat | `t02_pair.sh` | — |
| 2 | 1:1 inbound SMS → one portal, correct ghost | `t03_inbound_1to1.sh` | `IAMB_PHONE_ROOM` |
| 3 | Group create → exactly one room, no fracture | `t04_group_create.sh` | `IAMB_GROUP_ROOM` |
| 4 | Attachments byte-exact both ways; MMS size limit | `t05_attachments.sh` | `IAMB_PHONE_ROOM`, `ATTACHMENT_TARGET_NUMBER` |
| 5 | Group send → exactly one MMS row | `t06_group_send.sh` | `IAMB_GROUP_PORTAL` |
| 6 | Membership in place, same room id, no dupes | `t07_membership.sh` | `IAMB_GROUP_ROOM` |
| 7 | Tapbacks both ways | `t08_tapbacks.sh` | `IAMB_PORTAL_ROOM` + `IAMB_TAPBACK_EVENT` |
| 8 | Read state both ways | `t09_read_state.sh` | — (iamb has no receipt command) |
| 9 | Restart dedupe, recipients persist | `t10_restart_dedupe.sh` | `BRIDGE_RESTART_CMD`, `APP_RESTART` |
| 10 | Non-root device run (app transport) | `t11_nonroot.sh` | — |
| 11 | Multi-device / 2–4-participant combos | `t12_multidevice.sh` | — |
| 12 | Group reactions/uploads/replies + fallbacks | `t13_group_events.sh` | `IAMB_GROUP_PORTAL` |
| 13 | Invite→join flows | `t14_invite_join.sh` | `IAMB_GROUP_ROOM` |
| 14 | SMS message types (plain/long/unicode, both ways) | `t15_message_types.sh` | `IAMB_PHONE_ROOM`, `SMS_TARGET_NUMBER` |
| 15 | Animated GIF round-trip, byte-exact both ways | `t16_gif_roundtrip.sh` | `IAMB_PHONE_ROOM`, `MEDIA_TARGET_NUMBER` |
| 16 | Multi-attachment send → one MMS, all parts | `t17_multi_attachment.sh` | `MEDIA_TARGET_NUMBER`, `APP_HTTP_ENABLE_TEMP` (or `APP_BRIDGE_URL`) |
| 17 | Video/audio variants + degradation reporting | `t18_media_variants.sh` | `IAMB_PHONE_ROOM`, `MEDIA_TARGET_NUMBER` |
| 18 | Unknown-number thread/portal creation | `t19_unknown_sender.sh` | `IAMB_NEW_SENDER_ROOM` (optional) |
| 19 | Bridge CDN file service (crypto, links, expiry, quotas, revoke, isolation) | `t20_cdn.sh` | `CDN_BASE_URL` + `CDN_TOKEN` (env-gated) |

## Known gaps observed (2026-10-27 acceptance run)

These are acceptance findings, not harness bugs; they are recorded here so the next run
starts from the same baseline:

- **Inbound phone→Matrix message delivery**: portals exist with the right channel ids,
  ghosts and names, but no `m.room.message` event from the phone appears in them (SMS rows
  and MMS parts are present in the provider, and the app's event cursor advances). `t03`
  FAILs on this; `t13` stays consistent because no uploads were expected yet. The working
  tree carries an uncommitted cursor-reset fix (`connector/network_client.go`,
  `cursorAfterReset`); the deployed bridge predates it — re-run t03 after the
  fix is deployed.
- **Outbound MMS/group sends are refused while the app is not the default SMS app**
  (by design: `BridgeSender.rejectNonDefaultSmsMms`). `t05`/`t06` live probes SKIP with
  `default_sms_role=false`; `t08`'s outbound tapback additionally needs the bridge's
  `send_reactions_as_text` config.
- **Outgoing tapbacks** are off unless `send_reactions_as_text: true`; the reaction is
  otherwise stored in Matrix only (`t08` SKIPs, `EXPECT_REACTION_TEXT=1` asserts it).
