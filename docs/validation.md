<!--
SPDX-FileCopyrightText: 2026 Slavi Pantaleev

SPDX-License-Identifier: AGPL-3.0-or-later
-->

# Validation harness (`scripts/validate/`)

The per-repo end-to-end harness for the SMS/MMS bridge, the adb agent and the app fork.
It is env-driven: nothing assumes a specific workstation, phone or deployment. `t01` is the
smoke test that must report no FAILs before any of the acceptance tests run; `t02`–`t14`
follow the acceptance matrix in `todo.md` §3 and always assert real state (provider rows,
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
| `scripts/validate/t10_restart_dedupe.sh` | bridge DB dedupe + optional restart keeps portals/recipients |
| `scripts/validate/t11_nonroot.sh` | app transport works without root; agent root/non-root report |
| `scripts/validate/t12_multidevice.sh` | device inventory + 2/3/4-participant group combos |
| `scripts/validate/t13_group_events.sh` | group reactions/uploads/replies and SMS+MMS fallback evidence |
| `scripts/validate/t14_invite_join.sh` | no stray invites, management room/portals joined |
| `scripts/validate/fixtures/cat.gif` | tiny animated cat for the attachment tests (generated in-house) |
| `scripts/validate/fixtures/make-cat-gif.sh` | fixture generator + regeneration docs |

## Quick start

```sh
# 1. local dev rig (defaults, see docs/testing-e2e-local.md)
./scripts/validate/t01_local_rig.sh
./scripts/validate/t02_pair.sh

# 2. a deployment from the host the phone is plugged into, e.g. <ssh-host>:
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
| `ADB_SERIAL` | `<adb-serial>` | adb serial of the phone under test. |
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
| `AGENT_TOKEN` | `<agent-token>` | agent bearer token; `/v1/health` is auth-free. |
| `AGENT_INFO_PATH` | `/v1/health` | agent info path. |
| `CURL_TIMEOUT` | `10` | seconds per HTTP request. |
| `IAMB_SSH` | `<ssh-host>` | ssh host running iamb. |
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
| `MAX_GROUP_THREADS` / `MAX_CANDIDATE_ROOMS` | `3` / `6` | caps for slow iamb/portal scans. |

Tokens never belong in committed files; pass them via the environment (or a local,
gitignored `.env` sourced by your shell).

## Pointing at the local rig vs a deployment

- **Local rig** (defaults): bridge pairing API on `http://127.0.0.1:29322`, agent on
  `http://127.0.0.1:8093`, phone on adb. Nothing to set — see
  [testing-e2e-local.md](testing-e2e-local.md) to bring the rig up.
- **Deployment** (e.g. the test deployment; production is the current production): set
  `BRIDGE_URL=https://matrix.example.com`. Production `/_smsmms/` is served through the
  reverse proxy on 443, not on the container's `:29322` listener. The agent and adb checks
  only mean something on the host the phone is plugged into (the `<ssh-host>` host for the
  test phone), so set `ADB_SSH=<ssh-host>` and run the tests from a machine that can reach
  the bridge; `t01` SKIPs whatever is unreachable instead of failing.
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
  (this matches the tracker rule in `todo.md` §0).
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
primitives — no third-party artwork. `t05_attachments.sh` pushes it to the phone, checks
the shared→device and device→Matrix paths byte-exact (full SHA-256), and asserts it fits
`MMS_SIZE_LIMIT_BYTES`. `fixtures/README.md` documents provenance and regeneration.

## Test map (acceptance matrix, `todo.md` §3)

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
| 9 | Restart dedupe, recipients persist | `t10_restart_dedupe.sh` | `BRIDGE_RESTART_CMD` |
| 10 | Non-root device run (app transport) | `t11_nonroot.sh` | — |
| 11 | Multi-device / 2–4-participant combos | `t12_multidevice.sh` | — |
| 12 | Group reactions/uploads/replies + fallbacks | `t13_group_events.sh` | `IAMB_GROUP_PORTAL` |
| 13 | Invite→join flows | `t14_invite_join.sh` | `IAMB_GROUP_ROOM` |

## Known gaps observed (2026-10-27 acceptance run)

These are acceptance findings, not harness bugs; they are recorded here so the next run
starts from the same baseline:

- **Inbound phone→Matrix message delivery**: portals exist with the right channel ids,
  ghosts and names, but no `m.room.message` event from the phone appears in them (SMS rows
  and MMS parts are present in the provider, and the app's event cursor advances). `t03`
  FAILs on this; `t13` stays consistent because no uploads were expected yet. The working
  tree carries an uncommitted cursor-reset fix (`connector/network_client.go`,
  `cursorAfterReset`); the deployed test deployment bridge predates it — re-run t03 after the
  fix is deployed.
- **Outbound MMS/group sends are refused while the app is not the default SMS app**
  (by design: `BridgeSender.rejectNonDefaultSmsMms`). `t05`/`t06` live probes SKIP with
  `default_sms_role=false`; `t08`'s outbound tapback additionally needs the bridge's
  `send_reactions_as_text` config.
- **Outgoing tapbacks** are off unless `send_reactions_as_text: true`; the reaction is
  otherwise stored in Matrix only (`t08` SKIPs, `EXPECT_REACTION_TEXT=1` asserts it).
