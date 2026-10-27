# todo — SMS/MMS bridge + app: build-out & validation (adb-agent path dropped 2026-10-26)

> Resume-friendly tracker: tick `- [ ]` items as they complete. Keep evidence as IDs/senders/timestamps only — never log message bodies. Env-driven harnesses, no hardcoded local setup (this is a community project).

## 0. Conventions
- [ ] Deploy order: **the test deployment first** (number attached via test ssh host), validate, then production.
- [ ] LineageOS only (no GrapheneOS builds); degoogled Android focus, non-root paths must keep working (app transport; the adb-agent path was dropped 2026-10-26).
- [ ] Devices: adb serial `<adb-serial>` (US Mobile `+15550100100`, LineageOS 21, root) + localhost adb number + test-host number; iamb public/personal for JMP side.
- [ ] Harness rule: full `!room_id`s only (fuzzy names hang iamb); waits 60–120s with `--reply-mode different-sender`.
- [ ] Group correctness is the top invariant: any change that risks a second room, fractured portals, or own-number ghosts requires an acceptance-matrix re-run.

## 1. Control intents (automation enabler)
- [ ] `control_enabled` pref (default false) + trust gate (control token extra OR `android.permission.DUMP`).
- [ ] Ordered broadcasts with `setResultData(json)`: `control.STATUS`, `control.CONFIG`, `control.PAIR`, `control.UNPAIR`, `control.SEND`, `control.SYNC`.
- [ ] STATUS JSON: enabled, service running, paired device id/user, hs+bridge URLs, token present (never the token), port, default-SMS role, permissions, last heartbeat, queue+event cursors, outbox sizes, version.
- [ ] CONFIG: bridge_enabled, bridge_port, bridge_device_id, bridge_token, bridge_hs_url, bridge_base_url, control_enabled (applied prefs echoed back).
- [ ] PAIR: code + hs/base/device-id (or payload) → headless pairing via PairingClient/PairingStore; JSON device id/user.
- [ ] SEND: comma list + text (+ optional file) → one group-aware send (SMS 1 recipient; one group MMS for 2+/attachments); JSON {message_id, thread_id, pending}.
- [ ] SYNC: heartbeat + queue poll now.
- [ ] Unit tests for the JSON contracts.
- [ ] `tools/drive-app.sh <serial> status|pair|config|send|unpair|sync`.
- [ ] `docs/bridge-mode.md` (usage + security notes).

## 2. Per-repo e2e harness
- [x] `scripts/validate/lib.sh` env contract: `ADB_SERIAL`, `BRIDGE_URL`, `BRIDGE_TOKEN`, `AGENT_URL` (replaces `SMS_AGENT_URL`), `AGENT_TOKEN`, `SMSMMS_INFO_PATH`, `APP_PACKAGE`, `IAMB_*`/`JMP_*` pass-through (agent env vars unused — adb-agent path dropped 2026-10-26).
- [x] Scaffold landed 2026-10-26: `scripts/validate/t01_local_rig.sh` smoke test + `docs/validation.md`; helpers `curl_json`, `bridge_info`, `agent_info`, `drive_app`, `assert_agent_reachable`, `assert_paired`, `sha256_16`, `wait_for_room` (Matrix-side placeholder).
- [x] Helpers landed 2026-10-26/27: `adb_cmd`/`adb_shell` (local or `ADB_SSH`), `sqlite_query` + phone SMS/PDU/thread/part helpers, `sha256_file`/`sha256_compare`, `iamb_cmd`/`iamb_send`/`iamb_send_wait`/`room_for_number`/`archive_tail`, `portal_for_thread`/`send_and_wait`/`assert_one_room`, `prepare_attachment`/`control_send`.
- [x] Harness runs against the local dev rig **and** the test deployment (local: t01/t02/t10/t11; the test deployment + test phone: t02–t14, 2026-10-27).
- [x] Acceptance suite landed: `t02`–`t14` (pairing, inbound, group create, attachments, group send, membership, tapbacks, read state, restart dedupe, non-root, multi-device, group events, invite/join) + `fixtures/cat.gif`.

## 3. Acceptance matrix (all devices/modes)
- [x] 1. Pair (QR + `control.PAIR`) → `!sms devices` shows device; bridge heartbeat. (t02 PASS 2026-10-27: the test deployment + test phone, 1 device, fresh heartbeat.)
- [ ] 2. 1:1 inbound SMS → one portal room (30-day backfill), correct sender ghost, no own-number ghost. (t03: portal/channel ids/own-number filter PASS, but the portal carries no `m.room.message` from the phone — inbound content not delivered; fix then re-run.)
- [x] 3. Group create (iamb `create` group) → exactly ONE room with membership; second sender → no fracture. (t04 PASS with a live group probe: same phone thread, same room set, unique channel ids; portal names cover 1/2 participants — `PORTAL_STRICT_PARTICIPANTS=1` enforces full coverage.)
- [ ] 4. Attachments both ways byte-exact (SHA-256); MMS fits carrier limit. (t05: fixture + size limit + device round-trip + inbound MMS byte-exact PASS; outbound blocked by `default_sms_role=false`.)
- [ ] 5. Group send from Matrix → exactly one `send_mms`; both JMP numbers see it in the same thread. (t06 state PASS: group MMS rows present, one portal channel; live send blocked by `default_sms_role=false`.)
- [ ] 6. Membership add/drop in place, same room id, no dupes, no own-number ghost. (t07 state PASS: one room per channel id, no dupes, no own-number ghost; no add/drop event observed yet.)
- [ ] 7. Tapbacks both ways (documented fallback for outgoing). (t08: no inbound tapbacks observed; outgoing needs `send_reactions_as_text` — `EXPECT_REACTION_TEXT=1` asserts it.)
- [x] 8. Read state both ways. (t09 PASS: 6/6 inbound SMS `read=1 seen=1`; Matrix-side receipts are not scriptable with iamb.)
- [ ] 9. Restart dedupe (re-run step 2; no duplicates; recipients persist). (t10: local bridge DB dedupe PASS; restart action needs `BRIDGE_RESTART_CMD`.)
- [ ] 10. Non-root device run (app transport). (t11: test phone app transport healthy without root; a genuinely non-root device run is still pending.)
- [ ] 11. Multi-device matrix: localhost adb number + test phone adb number + iamb public/personal; group combos of 2/3/4 participants. (t12: 1 paired device, 2-participant combo verified; 3/4-participant combos not observed yet.)
- [ ] 12. Groups: reactions, uploads, replies, SMS+MMS fallback modes. (t13: phone/portal state consistent, but no reactions/uploads/replies observed on the group thread yet.)
- [x] 13. Invite→join flows (`create` via bridge, iamb invites). (t14 PASS: no pending invites, management room + portal candidates joined.)

## 4. Polish backlog
- [ ] Auto-fit attachment pipeline (resize/compress to MMS limit; `mmsFileSizeLimit` today is UI-only).
- [ ] Delivery states (sent/delivered receipts back into Matrix).
- [ ] Outgoing tapbacks.
- [ ] Large-file pointer hosting (cheogram-style link for oversized MMS) + compression prompt UX in the app.
- [ ] Release tagging checklist (bump VERSION_NAME/CODE, verify cert, Forgejo release, update `docs/PLAN.md`).

## 5. Deployments
- [ ] sms-mms → **the test deployment** (test ssh host number) first: deploy, check `/_smsmms/v1/info`, pair test phone.
- [ ] Run acceptance subset on the test deployment; fix; repeat.
- [ ] Then deploy to production (existing runbook + traps: preload `--network host`, as_token needs install-synapse after registration changes).
- [ ] jmpchat bridge: the test deployment only for tests (no production).

## 6. Calls follow-up (after A/B)
- [ ] See `mautrix-jmpchat/docs/SFRAME_CALLS_SPIKE.md` (M0–M4: research → Go SFrame codec → key receipt prototype → media round-trip → integration).

## Resolved decisions + new features (2026-10-26)

- Devices/numbers: the test deployment pairs the test phone +1-555-010-0200 (adb USB on `<ssh-host>`); localhost phone +1-555-010-0400 is a group-test number for now (future production account); localhost adb device <adb-serial>.
- [ ] Multi-number support: each paired number/account gets a nested space (bridge must support multiple numbers).
- [ ] Control intents stay intent-based (ordered broadcasts): driven from adb + Termux first; Tasker/Easer/other automation adapters optional later.
- [ ] Bridge-hosted large-file service ("CDN"): upload endpoint + public GET authenticated only by an unguessable hash in the URL; TTL expiration (default configurable, optional ask-per-upload); upload size limit; dumb HTML wrapper pages for image/audio/video/pdf ONLY (no HTML/SVG/JS payloads; nosniff; CSP sandbox) to avoid single-click exploits.
- [ ] App settings: ask-to-compress toggle; allow sending big files (or any files) as URLs toggle; URL expiration default + ask-each-time toggle; upload size limit.

## File service ("CDN") — resolved spec (2026-10-26, deferred until domains exist)

- [ ] Domains: `cdn.example.com` and `cdn.example.com` (user configures DNS; feature must be
      fully config-gated: no domain in the role => no service, same on the app side).
- [ ] Zero-knowledge: files are encrypted client-side (AES-GCM) before upload; the key lives in the
      URL fragment and never reaches the server; server stores ciphertext only ("encrypted at rest").
- [ ] Wrapper page per file: minimal static HTML+JS (WebCrypto) that decrypts via the fragment key and
      renders the raw browser-playable type (image/audio/video/pdf) from a blob URL. No HTML/SVG/JS
      payloads, `X-Content-Type-Options: nosniff`, CSP sandbox. No other frontend.
- [ ] Expiry: default 1 month; overrideable per upload and globally in app settings; reject > 1 year.
- [ ] URL is the secret: unguessable id in the path; GET needs no headers/cookies.
- [ ] Quotas: per-user upload cap (setting), FIFO pruning beyond the cap; intended only for
      avoiding SMS/MMS transport, not bulk storage.
- [ ] Revoke from the Matrix management room (list + revoke + stats), own files only.
- [ ] App settings: ask-to-compress toggle; allow big/any files as URLs toggle; expiry default +
      ask-each-time toggle; upload size limit; global expiry override.
- [x] Transport (2026-10-26): app pairing only — the adb-agent path was dropped as too complex; the bridge uses the app transport.
      for exhaustive testing (no hard requirement).
