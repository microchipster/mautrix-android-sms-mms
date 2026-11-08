# TODO - SMS/MMS bridge + app: outstanding work

> Completed work is archived in `docs/DEVELOPMENT.md`; operational state and runbooks in
> `docs/sms-bridge-handoff.md`. Keep evidence as IDs/senders/timestamps only - never log
> message bodies. Deploy/validation conventions live in `docs/DEVELOPMENT.md`.

## Acceptance matrix (open items)

Numbering matches the original matrix; passed items are archived in `docs/DEVELOPMENT.md`.

- [ ] 2. 1:1 inbound SMS -> one portal room (30-day backfill), correct sender ghost, no own-number ghost. (Portal/channel ids/own-number filter PASS; the portal carries no `m.room.message` from the phone - inbound content not delivered; fix then re-run.)
- [ ] 4. Attachments both ways byte-exact (SHA-256); MMS fits carrier limit. (Fixture + size limit + device round-trip + inbound MMS byte-exact PASS; outbound blocked by `default_sms_role=false`.)
- [ ] 5. Group send from Matrix -> exactly one `send_mms`; both JMP numbers see it in the same thread. (State PASS; live send blocked by `default_sms_role=false`.)
- [ ] 6. Membership add/drop in place, same room id, no dupes, no own-number ghost. (State PASS; no add/drop event observed yet.)
- [ ] 7. Tapbacks both ways (documented fallback for outgoing). (No inbound tapbacks observed; outgoing needs `send_reactions_as_text` - `EXPECT_REACTION_TEXT=1` asserts it.)
- [ ] 9. Restart dedupe (re-run step 2; no duplicates; recipients persist). (Local bridge DB dedupe PASS; restart action needs `BRIDGE_RESTART_CMD`.)
- [ ] 10. Non-root device run (app transport). (Test phone app transport healthy without root; a genuinely non-root device run is still pending.)
- [ ] 11. Multi-device matrix: localhost adb number + test phone adb number + iamb public/personal; group combos of 2/3/4 participants. (1 paired device, 2-participant combo verified; 3/4-participant combos not observed yet.)
- [ ] 12. Groups: reactions, uploads, replies, SMS+MMS fallback modes. (Phone/portal state consistent; no reactions/uploads/replies observed on the group thread yet.)

## Polish backlog

- [ ] Auto-fit attachment pipeline (resize/compress to the MMS limit; `mmsFileSizeLimit` today is UI-only).
- [ ] Delivery states (sent/delivered receipts back into Matrix).
- [ ] Outgoing tapbacks.
- [ ] Compression prompt UX on top of the CDN service; oversized-MMS pointer hosting.
- [ ] Optional automation adapters (Tasker/Easer) on top of the control-intent broadcasts.
- [ ] Release tagging: first version tags + CHANGELOG entries for the bridge; keep the app release checklist (bump VERSION_NAME/CODE, verify cert, publish release).

## Deployments

- [ ] sms-mms -> the test deployment (number on the test ssh host) first: deploy, check `/_smsmms/v1/info`, pair test phone.
- [ ] Run the acceptance subset on the test deployment; fix; repeat.
- [ ] Then deploy to production (runbook traps: preload `--network host`; as_token needs install-synapse after registration changes).
- [ ] jmpchat bridge: the test deployment only for tests (no production).

## Multi-number support

- [ ] Each paired number/account gets a nested space (bridge must support multiple numbers).

## Calls follow-up

- [ ] SFrame/E2EE calls spike (M0-M4) - see `mautrix-jmpchat/docs/SFRAME_CALLS_SPIKE.md`.
- [ ] Forward calls from a rooted LineageOS device (USB/adb) to Matrix SIP directly, for making and receiving real calls via Matrix clients.
