<!--
SPDX-FileCopyrightText: 2026 Slavi Pantaleev

SPDX-License-Identifier: AGPL-3.0-or-later
-->

# mautrix-android-sms-mms

A Matrix bridge for Android **SMS and MMS** — with real group chats, attachments
and reactions — without Google services. Built for DeGoogled devices
(GrapheneOS, LineageOS); RCS is intentionally not supported.

Your phone's SMS/MMS threads become Matrix rooms. One room per phone thread,
kept stable for the life of the thread: group MMS participants update in place,
attachments arrive as normal Matrix images/files, incoming tapbacks become
Matrix reactions, and reading a room marks the thread read on the phone.

## How a phone connects

Two transports, same bridge:

| Transport | What it needs | What it gives you |
|---|---|---|
| **App mode** (recommended) | The [Fossify Messages bridge app](https://git.example.com/repos/fossify-messages-bridge/releases) on the phone, paired to your homeserver over HTTPS | Everything: incoming and outgoing SMS/MMS, group MMS, attachments, read-state sync. Works from anywhere — no cables, no LAN. |
| **ADB agent mode** | A computer with the phone plugged in over USB, `adb`, optional root | SMS/MMS reading, SMS sending, attachments. MMS **sending** is not possible over ADB — use app mode if you need it. |

Both transports identify the phone the same way, so a phone can move between
them without creating duplicate rooms.

## Requirements

- A Matrix homeserver (Synapse) and this bridge deployed and registered as an
  appservice. With [matrix-docker-ansible-deploy](https://github.com/spantaleev/matrix-docker-ansible-deploy)
  this is one variable and one deploy (see below).
- A phone running Android with a SIM. For app mode: any Android 8+ device, no
  root needed. For ADB mode: USB debugging enabled.
- The bridge must be reachable over HTTPS from the phone for app mode. The
  pairing API is served under `/_smsmms/` on your homeserver. If you deploy
  with the playbook, this is wired up automatically.

## Quick start: app mode

### 1. Deploy the bridge

With matrix-docker-ansible-deploy, in your `vars.yml`:

```yaml
matrix_bridge_mautrix_android_sms_mms_enabled: true
```

then run the playbook for the `install-mautrix-android-sms-mms` tag (or the
usual setup/install tags). The role self-builds the bridge from this repository,
serves `/_smsmms/` through Traefik, and advertises the bridge URL in
`.well-known/matrix/client` (`chat.example.bridge.sms_mms`) so the app can
discover it automatically.

Not using the playbook? `docs/AGENT_API.md` and `docs/PAIRING_PROTOCOL.md`
describe the two HTTP surfaces; run the bridge with `-c config.yaml`, register
`registration.yaml` on your homeserver, and reverse-proxy `/_smsmms/` to the
pairing listener (default `:29322`).

### 2. Install the app

Grab the signed APK from the
[releases page](https://git.example.com/repos/fossify-messages-bridge/releases):

```sh
adb install -r MessagesBridge-<version>.apk
```

Open **Messages Bridge** and grant the SMS permissions. Then set it as the
**default SMS app** — required for MMS and read-state sync (plain SMS works
without it). It installs side by side with regular Fossify Messages.

### 3. Pair the phone

In the management room of the bridge, run:

```
!sms login app-pair
```

The bot posts a QR code and a short code. In the app, open
**Settings → Bridge pairing**:

- scan the QR code, **or**
- paste the pairing link, **or**
- enter the homeserver URL and the short code manually.

The app stores a device token (kept encrypted on the phone) and starts syncing.
That's it — you should see a room per SMS/MMS thread appear within seconds.

### 4. Use it

- **Send**: type in the thread's room; it goes out as SMS (or MMS when there
  are attachments or multiple recipients) from the phone.
- **Receive**: nothing to do — new messages appear in their rooms.
- **Read state**: reading a room marks the phone thread read, and vice versa.
- **History**: on first login the bridge backfills the last 30 days; use
  `!sms sync 90d` or `!sms sync full` for more.

## Quick start: ADB agent mode

On the computer the phone is plugged into:

```sh
./build-agent.sh
./android-sms-mms-adb-agent -e > agent.yaml   # example config
$EDITOR agent.yaml                            # token, own_numbers, serial
./android-sms-mms-adb-agent -c agent.yaml
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8093/v1/health
```

Then create the bridge login with the agent URL and token (`!sms login` →
paste the URL/token when prompted). Keep the agent running — it polls the
telephony database over ADB, so it needs the phone attached.

Notes:

- Root is optional: on AOSP-derived ROMs (GrapheneOS, LineageOS) `com.android.shell`
  already holds the SMS permissions needed for reading and sending SMS.
- MMS sending requires the app; the agent reports this (health
  `send_mms: false`) and the bridge tells you when you try.

## Bridge commands

All commands live in the management room (default prefix `!sms`):

| Command | What it does |
|---|---|
| `!sms help` | List everything. |
| `!sms ping` | Check the phone connection. |
| `!sms sync`, `!sms sync 30d`, `!sms sync full` | Backfill history. |
| `!sms sync-status` | Show per-room sync progress. |
| `!sms stats [24h\|7d\|30d\|all]` | Message counts by direction, SMS/MMS and carrier/SIM. |
| `!sms label <phone> <label\|--clear>`, `!sms labels` | Tag contacts (auto-labels first-time senders) and list tags. |
| `!sms block-user`, `!sms unblock-user` | In a DM room: drop inbound messages from that sender, bridge-side. |
| `!sms ttl status`, `!sms ttl run [--dry-run\|--apply]` | Show or run the Matrix history retention cleanup. |
| `!sms merge <room> <room> [...] [--into <room>] [--yes]`, `!sms merges`, `!sms unmerge <room>` | Combine fractured rooms (one conversation split across threads) into one target room. |
| `!sms devices` | List paired devices and their state. |
| `!sms unpair <device_id\|all>` | Revoke a device's token. |
| `!sms rebind-room <thread\|address> <room_id>` | Attach a phone thread to an existing Matrix room. |
| `!sms list-logins`, `!sms login` | Manage logins. |

## Troubleshooting

- **No rooms appear after pairing** — check the app shows a paired device in
  its settings, and that `!sms devices` lists it as connected. If the app can't
  reach the bridge, set the bridge URL manually from the pairing screen.
- **"MMS sending is not supported"** — the app is not the default SMS app
  (or you're in ADB agent mode). Fix the default-app setting for app mode.
- **A message was delivered as text only** — the attachment could not be read.
  In app mode, very large attachments over the inline size cap are skipped with
  a warning; in ADB mode a missing part file does the same.
- **Your own number appears as a participant** — tell the bridge your numbers:
  app mode learns them from the phone automatically; in ADB mode set
  `own_numbers` in `agent.yaml`, or set `network.own_numbers` in the bridge's
  `simple-config.yaml`.
- **Pairing code expired** — codes are single-use and valid for 10 minutes;
  run `!sms login app-pair` again.

## Repository layout

```
cmd/mautrix-android-sms-mms/     the bridge (bridgev2 appservice)
cmd/android-sms-mms-adb-agent/   the ADB agent
internal/agent/                  agent internals (provider, poller, send, HTTP API)
connector/                       bridge connector (logins, portals, media, pairing)
tools/pairsim/                   simulated phone for local end-to-end tests
docs/                            plan, protocols, references, runbooks
```

Build and test:

```sh
go build ./... && go vet ./... && go test ./...
./build-go.sh          # bridge binary
./build-agent.sh       # agent binary
docker build -t mautrix-android-sms-mms .   # container image
```

## Docs

- [`docs/PLAN.md`](docs/PLAN.md) — the plan, phases and the group-chat correctness spec.
- [`docs/sms-bridge-handoff.md`](docs/sms-bridge-handoff.md) — full project handoff: state, runbooks, testing rig, next goals.
- [`docs/PAIRING_PROTOCOL.md`](docs/PAIRING_PROTOCOL.md) — app pairing protocol (QR + manual, queue/events API).
- [`docs/AGENT_API.md`](docs/AGENT_API.md) — HTTP API contract both transports implement.
- [`docs/carrier-scoped-stats.md`](docs/carrier-scoped-stats.md) — `!sms stats`, carrier/SIM aggregation and ad-hoc SQL views.
- [`docs/caller-id-and-labels.md`](docs/caller-id-and-labels.md) — what caller info SMS has, labels, new-contact tagging and anti-spam.
- [`docs/history-ttl.md`](docs/history-ttl.md) — Matrix history retention: config schema, pattern presets and cleanup behavior.
- [`docs/merged-chats.md`](docs/merged-chats.md) — merging fractured conversations: registry, routing, tombstones and validation.
- [`docs/testing-e2e-local.md`](docs/testing-e2e-local.md) — local end-to-end test runbook.
- [`docs/SIGNING.md`](docs/SIGNING.md) — how releases are signed.
- [`docs/research-adb-reference.md`](docs/research-adb-reference.md), [`docs/research-app-base.md`](docs/research-app-base.md) — research notes.

## License

AGPL-3.0-or-later.
