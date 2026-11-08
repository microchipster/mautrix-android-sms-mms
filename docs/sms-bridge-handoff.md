<!--
SPDX-FileCopyrightText: 2026 Slavi Pantaleev

SPDX-License-Identifier: AGPL-3.0-or-later
-->

# SMS bridge handoff

This document hands the `mautrix-android-sms-mms` project to a fresh session. It is the
map: what exists, what is live, what is left, and how to drive every piece. Read it top to
bottom once, then use it as a runbook.

Everything below was verified from this workstation unless marked "expected".

## 1. What this project is

A Matrix bridge for **SMS and MMS**, built as two programs in one repository:

| Component | Binary | Runs on | Talks to the phone via |
|---|---|---|---|
| Bridge | `mautrix-android-sms-mms` | the Matrix server (Docker + systemd, MDAD playbook) | the app or the adb agent over HTTP |
| ADB agent | `android-sms-mms-adb-agent` | a computer with the phone attached over USB/adb | `content` provider queries + `service call isms` |
| App (fork) | `org.fossify.messages.bridge` (Fossify Messages fork) | the phone itself | Android telephony APIs |

Goal in the user's words: group chats must be **perfect** (no fractured single-SMS rooms),
attachments/images byte-exact, one stable Matrix room per phone thread, Android-only
approaches (DeGoogled; no Google account, no RCS).

The bridge speaks **one wire protocol** to both transports (`docs/AGENT_API.md` for the
agent's HTTP API, `docs/PAIRING_PROTOCOL.md` for the app's queue/events API). The app is a
drop-in replacement for the agent, and both must produce identical room/ghost identities
(via `bridge_device_id` = the adb serial).

## 2. Repositories, state, remotes

| Repo | Path (this machine) | Branch | Remote |
|---|---|---|---|
| Bridge + agent | `~/src/repos/matrix-docker-ansible-deploy/mautrix-android-sms-mms` | `main` | `git@github.com:microchipster/mautrix-android-sms-mms.git` |
| App fork (Fossify Messages) | `~/src/repos/fossify-messages` | `bridge` | `github.com/microchipster/fossify-messages-bridge` (upstream = `FossifyOrg/Messages`) |
| mmslib fork | `~/src/repos/fossify-mmslib` | `bridge` | `github.com/microchipster/fossify-mmslib-bridge` (upstream = `FossifyOrg/android-smsmms`) |
| Playbook (MDAD) | `~/src/repos/matrix-docker-ansible-deploy` (dev, dirty allowed) | `master` | `github.com/microchipster/matrix-docker-ansible-deploy` |
| Playbook (deploy clone) | `~/src/other/ansible/spantaleev/matrix-docker-ansible-deploy` (pristine) | `master` | same, force-pushed before deploys |
| Private deploy vars | `~/src/repos/private/cloud/matrix_ansible/inventory/host_vars/matrix.example.com/vars.yml` | - | separate private repo, user edits in flight - do not commit others' changes |

Development happens on a self-hosted Forgejo instance; the repository is mirrored to
GitHub (`github.com/microchipster/mautrix-android-sms-mms`). Hosting credentials live
outside the repository and are never committed.

Branches in the bridge repo (oldest → newest, all pushed): `e46460c` initial project,
`1bcc8fa` pairing API, `21bf25b` app-pair login flow, `4e79a5e` app transport routing,
`21db444` docs, `3686cb5` pairing protocol doc, `63d83ba` PLAN phases, `694d7e5` logger fix,
`2a491e4` pairsim, `86f4bd7` E2E runbook, `761ed21` encryption gate + ack pruning, `fec2f6d`
ack docs, `073796e` merged-address split, `82add42` inline base64 attachments, `4ffa424`
nil-agent guard, `7b0fc03` command_seq, `d360ed7` own numbers from heartbeats, `4af9572`
pairsim group seeds, `7e18932` gitignore, `6878508` E2E group docs, `3af74ce` PLAN,
`c06ed41` re-apply subscriptions, `8b05f53` self-thread recipients, `f543f00` thread.updated
intake, `4e5d03f` sub_id fix + portal fallback, `0f042f6` PLAN, `e5d2b86` Dockerfile,
`2a3146c` entrypoint args, `092bba2` su-exec conditional, `fd0b8f2` README, `b4cd49c`
public homeserver URL, `d08f599` PLAN, `a0128bc` SIGNING, `bd0f640` signing backup path,
`567ef45`/`8ec09c0` twilio tool, `401d98d` twilio removal.

App branch `bridge` head: `b6e61d61` (script hardening) on top of `b4cd49c`-era work:
pairing client (`625de03`, `c3fa75d`, `c56bc3c`, `52abcef`, `21fc40e`, `238bf8a`),
deep link + device id (`b228909`), `b724a34` thread.updated recipients, `bb612918` release
version. Release tag `v1.10.1-bridge.1`; mmslib tag `v1.0.0-bridge.1`.

## 3. Architecture in one screen

- **Bridge** is a bridgev2 network connector (`connector/`): portal IDs
  `thread-<deviceScope>-<subID>-<threadID>` (stable per phone thread → one room per thread,
  never fractured), ghosts from escaped phone numbers (`=2b` for `+`, `=2c` for `,`),
  sender attribution (empty sender = outgoing), tapback texts → Matrix reactions,
  history sync with progress markers, `!sms help|ping|sync|sync-status|list-logins|login|devices|unpair|rebind-room|delete-all-portals|logout`.
- **Agent** (`internal/agent/` + `cmd/android-sms-mms-adb-agent`): polls the telephony
  provider over adb (`content query`/`read`/`update`), sends SMS with `service call isms`
  (per-SDK transaction codes), serves `docs/AGENT_API.md`. Works **without root**
  (`com.android.shell` holds SMS permissions); root only adds DB/file access. MMS **send**
  is impossible over adb (needs an app) - the agent's health reports `send_mms: false` and
  the bridge gates features on it.
- **App** (`app/src/main/kotlin/org/fossify/messages/bridge/` in the fork):
  `BridgeService` foreground service = the same HTTP API as the agent for local/TCP use,
  plus `PairingClient`/`PairingPoller` that keep a long-poll to the bridge's
  `/_smsmms/v1/queue` and push `/_smsmms/v1/events` (NAT-friendly: the phone dials out).
  Pairing: `!sms login app-pair` → QR/manual code → ECDSA P-256 device key signs a nonce →
  per-device bearer token (SHA-256 at rest). `docs/PAIRING_PROTOCOL.md` is canonical.
- **Own numbers**: the phone's own number must never become a ghost/participant. Sources:
  agent `own_numbers` config, app heartbeat `subscriptions`, bridge `own_numbers` fallback;
  self-threads keep the own number so they stay sendable.
- **Group correctness invariants** (do not break): one portal per thread id; membership
  updates in place via `ChatInfoChange` (a thread rebuild must not create a second room);
  multi-recipient Matrix sends → **one** `send_sms`/`send_mms` command (the app sends one
  group MMS with all attachments); attachments arrive as base64 inline in `message.new`
  on the app transport (≤10 MiB encoded, oversized parts omitted) or via
  `GET /v1/mms/{id}/parts/{partID}` on the agent transport; `thread.updated` may carry
  `recipients` (repairs portals after restarts; `sub_id: -1` means unknown).

## 4. Production status (live on example.com)

Deployed 2026-10-23 via MDAD tags `install-synapse,install-mautrix-android-sms-mms,start`.
Verified:

- `https://matrix.example.com/_smsmms/v1/info` →
  `{"protocol_version":1,"bridge_name":"mautrix-android-sms-mms","homeserver_url":"https://matrix.example.com","pairing_enabled":true}`
  (Traefik routes `/_smsmms/` → container `:29322`; `.well-known` key
  `chat.mdad.bridge.sms_mms` is published by the playbook).
- Service `matrix-mautrix-android-sms-mms.service` active; container `matrix-mautrix-android-sms-mms`
  runs with the playbook-provided command line; appservice token accepted; bot
  `@android-sms-mmsbot:example.com` syncing.
- Nothing paired yet on production (login count 0). Pairing the real phone is the user's
  next step: `!sms login app-pair` → scan QR (app: Settings → Bridge pairing).

### Deploy runbook

```sh
# 1. bridge repo: commit + push main (playbook preloads from the Forgejo repo)
# 2. playbook dev repo: commit, rebase on github/master, force-push origin
# 3. pristine clone: git fetch origin && git reset --hard origin/master
# 4. deploy (tags: only the bridge; add install-synapse only when synapse-side
#    config/registration changes are needed)
cd /tmp/opencode && nohup env SERVICE=matrix CRONRUN_CHECK_HOST=<ssh-host> \
  /home/user/bin/shell/cronrun-online \
  'update-matrix-server example.com matrix matrix install-mautrix-android-sms-mms,start ""' \
  > /tmp/opencode/deploy-sms-mmsN.log 2>&1 &
```

Traps (all hit and solved once - don't re-solve them):

- The preload build on the controller needs `--network host` (docker DNS inside containers
  is unreliable here). The playbook already does this.
- Adding or changing an appservice registration requires `install-synapse` once (regenerates
  Synapse config + mounts `registration.yaml` into the synapse container). Bridge-only
  deploys will otherwise fail with "The as_token was not accepted".
- The managed Postgres DB (`matrix_bridge_mautrix_android_sms_mms`) was created **manually**
  with the playbook's own SQL semantics (idempotent; the postgres role is only tagged
  `install-postgres`/`setup-postgres`). Re-running `install-postgres` later is safe.
- `matrix-synapse-worker-generic-1.service` was missing (private vars ask for 2 generic
  workers); `install-synapse` created it. Do not "fix" by lowering the worker count.
- Container entrypoint (`docker-run.sh`) honours the playbook's command line and only uses
  `su-exec` when the container starts as root (`--user=991:991 --cap-drop=ALL`).
- Release notifications: `nohup … cronrun-online` posts failures to the user's hookshot
  webhook via `matrix_notify`. Watch a deploy with a background
  `while kill -0 <pid>; do sleep 20; done` and tail the log.

## 5. Local devices and harnesses

- **Phone**: Pixel 2 XL, adb serial `<adb-serial>`, LineageOS 21 (Android 14/SDK 34),
  Magisk root, **unlocked with no password - leave it that way**. SIM sub_id 1,
  US Mobile `+15550100100`.
- **Release app** `org.fossify.messages.bridge`: installed from the pass-signed APK, holds
  the default-SMS role, paired to the local E2E bridge for tests. Not debuggable (no
  `run-as`).
- **Debug app** `org.fossify.messages.bridge.debug`: side-by-side, prefs
  `bridge_enabled=true`, `bridge_token=0123456789abcdef0123456789abcdef`, `bridge_port=8391`,
  `bridge_device_id=<adb-serial>`; reachable via
  `adb forward tcp:18391 tcp:8391`; used for inspection because `run-as` works on it.
- **Local E2E rig** (no cloud needed): Synapse container `e2e-synapse` on
  `http://localhost:8008`; bridge in `/tmp/opencode/e2e-pairing/bridge` (port 29322);
  simulator `tools/pairsim` (`pairsim -code <CODE> -device-id … -seed-mms seed.png …`);
  helpers `/tmp/opencode/e2e-pairing/{token.txt (line 2 = token), room_id.txt, send-msg.sh, seed.png}`;
  runbook `docs/testing-e2e-local.md`. Quirks: kill listeners via
  `ss -ltnpH 'sport = :29322'` (never `pkill -f` with a pattern that matches your own
  command line), use a fresh log filename per bridge restart, pairing codes are
  ANSI-stripped from the log ("manually: <CODE>").
- **Agent test instance**: `http://127.0.0.1:8093` (token `<agent-token>`, state
  `/tmp/opencode/sms-agent/data/`, rebuild with
  `go build -o /tmp/opencode/bin/sms-agent ./cmd/android-sms-mms-adb-agent`, restart with
  `bash /tmp/opencode/start-agent.sh`). Never `pkill -f sms-agent` - it kills the invoking
  shell.
- **Signing**: the canonical flow is `scripts/build-release.sh` in the app repo
  (`PASS_NAMESPACE=<user>`, keystore in `~/.password-store/<user>/android/org.fossify.messages.bridge/`,
  password cached at `~/.config/android/org.fossify.messages.bridge/storepass`, GPG archive
  `signing-backup-20261023.tar.gz.gpg` in the same directory). Missing keystore is a hard
  error; generating needs `--generate-key` (never regenerate - breaks APK updates). `pass`
  writes work without unlocking GPG; reads need an unlocked agent. `docs/SIGNING.md` has the
  full procedure, cert fingerprint `678d78dd…b34b`, and the restore steps.

## 6. New goal (requested): full intent automation of the app

Today the app can only be driven by taps or by the debug-only `run-as` path (plus the
exported `smsmms://pair` deep link). The user wants the app **fully automatable from
adb/Termux/any shell without root**. Implement a small, token-gated "control intents"
surface in the fork.

Why it matters: the other session can then script the whole test matrix (configure, pair,
send, read state, unpair) on a release build, and Termux users can re-pair without touching
the screen.

Suggested surface (keep it minimal and documented in `docs/bridge-mode.md`):

| Intent action | Extras | Result |
|---|---|---|
| `org.fossify.messages.bridge.control.STATUS` | - | JSON: enabled, service running, paired device id/user, hs + bridge URLs, token present (never the token), port, default-SMS role, runtime permissions, last heartbeat, queue/event cursors, outbox sizes, app version |
| `…control.CONFIG` | `bridge_enabled`, `bridge_port`, `bridge_device_id`, `bridge_token`, `bridge_hs_url`, `bridge_base_url`, `control_enabled` | applied prefs echoed as JSON |
| `…control.PAIR` | `code`, `hs`, `b`, `device_id` (or `payload`) | pairing performed headlessly; JSON with device id/user |
| `…control.UNPAIR` | - | pairing cleared |
| `…control.SEND` | `to` (comma list), `text`, optional `file` (path readable by the app) | one group-aware send (SMS for 1 recipient, one group MMS for 2+/attachments); JSON `{message_id, thread_id, pending}` |
| `…control.SYNC` | - | heartbeat + queue poll kicked now |

Implementation notes discovered while building the app side:

- Prefer **ordered broadcasts** with `setResultData(json)`: `am broadcast` prints the data,
  which is the simplest shell contract (works from adb and Termux).
- Gate trust two ways: accept when the sender either (a) holds the control token passed in
  an extra, or (b) has `android.permission.DUMP` (adb shell does; most apps don't). This
  keeps `am broadcast` clean while Termux (no `DUMP`) can still use the token.
- Keep the receiver inert unless `control_enabled` is true (default false) so a stock
  install is not remotely drivable.
- `PAIR` should reuse `PairingClient`/`PairingStore` (the activity only does UI around them);
  pairing must not require the foreground activity.
- `SEND` must reuse `BridgeSender` (group flag logic, one-MMS rule) so behaviour matches
  the bridged path exactly.
- Add unit tests for the JSON contracts + a device script (e.g.
  `tools/drive-app.sh <serial> status|pair|config|send|unpair|sync`) in the bridge repo,
  and document the surface in the app's `docs/bridge-mode.md`.
- Keep the app compiling with no new dependencies; CI is `detekt` + `lintFossDebug` +
  `assembleFossDebug`.

## 7. Testing plan with the JMP.chat / iamb rig (replaces Twilio)

The user's testing scheme is documented in **`jmpchat-iamb.md`** at the playbook repo root
(`~/src/repos/matrix-docker-ansible-deploy/jmpchat-iamb.md`). Essentials:

- `<ssh-host>` runs iamb daemons for two profiles: `-P public` (JMP.chat SMS/MMS bridge,
  rooms named by phone number, e.g. `+15550100500@cheogram.com`) and `-P personal`
  (JMPChat group chats; management room `jmpchat bridge bot` =
  `!management-room:example.com`). The file lists ~57 JMP numbers with
  their room ids - enough "friends" to build group MMS threads without humans.
- Send + wait for a reply with JSON (use `--reply-mode different-sender`, timeout 90-120 s):
  `ssh <ssh-host> "~/.cargo/bin/iamb -P public send --room '$ROOM' --stdin --wait --timeout 90 --reply-mode different-sender --wait-output json"`.
- Read inbound via the archive tail (`~/.local/share/iamb/profiles/public/events/<room-without-!>.ndjson`;
  the `personal` profile has the same files without the `.ndjson` extension).
- New conversations/groups must be created **by the bridge**: send `create <jid>` /
  `create +1…,+1…@cheogram.com` (ascending, comma-joined) in the management room, never
  `iamb dm` (that creates unbridged rooms). Capture the reply with the same `--wait` flags.
- Caveats: pass full `!room_id`s only (fuzzy names hang on prompts); one room per number;
  never open the big JMPChat space in the TUI; the public profile prints `Bridge: jmpchat`
  in `room info` while the personal one does not; JMP terms forbid automation/marketing -
  keep it low-volume, sanctioned testing only; never log message bodies.

Acceptance matrix to run against the production bridge (each step: what to do → evidence):

1. **Pair** the phone (`!sms login app-pair`, QR or `… control.PAIR`) → `!sms devices` shows
   the device; bridge log shows heartbeat.
2. **1:1 inbound**: a JMP number texts the phone → one portal room appears (30-day
   backfill), sender ghost correct, no own-number ghost.
3. **Group create**: JMP number A sends a group MMS to the phone + B (or `create` the group
   on the JMP side) → exactly **one** room whose membership is phone + A + B; send a second
   message from B → still one room (no fracture).
4. **Attachments both ways**: image from a JMP number → lands as `m.image` (byte-exact;
   compare SHA-256 with the source); image from Matrix → phone receives one MMS, verify on
   a JMP number's inbox.
5. **Group send from Matrix** to A+B → exactly one `send_mms` command, one MMS at the
   carrier (both JMP numbers see it in the same thread).
6. **Membership change**: add C / drop B → same room id, in-place membership update, no
   duplicate rooms, no own-number ghost.
7. **Tapback**: A replies "Liked “…”" → Matrix reaction appears; Matrix reaction → text
   fallback (if enabled) or documented drop.
8. **Read state**: read in Matrix → phone thread marked read (`content query` shows
   `read=1 seen=1`); read on the phone → Matrix receipt.
9. **Restart dedupe**: restart the bridge (service restart) and re-run step 2 → no duplicate
   events; portal metadata still has recipients.
10. **GrapheneOS run**: repeat 1-5 on the non-root device (agent transport or app transport
    - both must work without root).

Record evidence in the PR/issue as ids, timestamps and hashes only.

## 8. Remaining backlog (beyond the new intent goal)

- Auto-fit attachment pipeline: resize/compress to the carrier MMS limit (`mmsFileSizeLimit`
  is UI-only today; the bridge sends what it gets).
- Delivery states (sent/delivered receipts back into Matrix) - command `status` exists in
  the protocol but the app does not emit updates yet.
- Tapback round trip (incoming done; decide the outgoing story).
- GrapheneOS non-root acceptance run on hardware (the user has a GrapheneOS phone; the adb
  agent path must be validated there because root is impossible).
- Release tagging checklist (bump `VERSION_NAME/CODE`, build, verify cert, create the
  Forgejo release, update `docs/PLAN.md`).
- Optional: single-message `getMessage(id)`-style endpoints are already satisfied by
  `/v1/messages/{id}`; keep the agent and app APIs locked together (update
  `docs/AGENT_API.md` + `docs/PAIRING_PROTOCOL.md` in the same commit as code).

## 9. Invariants and safety rails

- **Do not** change portal-ID derivation without a migration plan; rooms must survive
  app/agent switches (`bridge_device_id` must equal the adb serial; changing it on a live
  deployment creates a second set of rooms).
- **Do not** regenerate signing keys; a missing keystore is a restore problem
  (`docs/SIGNING.md`).
- **Do not** commit from the private deploy repo; it has in-flight user edits.
- **Do not** run destructive adb commands on the phone; it holds real SMS history. Read
  paths are fine (`content query`/`read`), writes only through the app/agent APIs.
- Keep the user's device unlocked/no-password as-is.
- Group correctness is the top priority: any change that could create a second room for an
  existing thread, or drop the own-number filter, needs the acceptance matrix re-run.
